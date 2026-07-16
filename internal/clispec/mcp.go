package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"goodkind.io/clyde/internal/response"
)

// Do not replace the per-parameter decoding below with
// mcp.CallToolRequest.BindArguments. That helper takes an any value and fills
// a struct by reflection over struct tags, which reintroduces the loose,
// untyped boundary this package exists to avoid. Read each input through the
// typed Require/Get accessors instead.

// mcpDescription builds the tool description from the one-line summary, the
// longer description, and the example command lines, so a caller with no help
// screen reads the same guidance the terminal shows.
func (op Operation[I, P]) mcpDescription() string {
	var b strings.Builder
	b.WriteString(op.Short)
	if op.Long != "" {
		b.WriteString("\n\n")
		b.WriteString(op.Long)
	}
	if len(op.Examples) > 0 {
		b.WriteString("\n\nExamples:\n")
		b.WriteString(strings.Join(op.Examples, "\n"))
	}
	return b.String()
}

// mcpTool renders the operation as an MCP tool plus its handler. The tool
// schema comes from the arguments and parameters; the handler decodes the same
// request into a fresh input struct and runs the shared work function.
func (op Operation[I, P]) mcpTool() (mcp.Tool, server.ToolHandlerFunc) {
	tool := mcp.NewTool(op.Name.MCP(), op.mcpToolOptions()...)

	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var callerDirectoryErr error
		ctx, callerDirectoryErr = withMCPCallerWorkingDirectory(ctx, req)
		if callerDirectoryErr != nil && op.Name.Canonical == "export_transcript" {
			return newMCPTextResult(ctx, callerDirectoryErr.Error()), nil
		}
		in := op.New()
		missingArg := bindMCPArgs(&in, op.Args, req)
		if missingArg != "" {
			return newMCPTextResult(ctx, missingArg+" is required"), nil
		}
		bindMCPParams(&in, op.Params, req)
		// Prepare is the only place input is rejected. It runs after binding and
		// before the work, so an input error becomes the tool result text instead
		// of reaching the work function. This mirrors the terminal, where Prepare
		// runs in PreRunE before the help boundary.
		prepared, prepErr := op.Prepare(in)
		if prepErr != nil {
			wrapped := logFail(ctx, SurfaceMCP, "invalid_input", op.Name.Canonical, prepErr)
			return newMCPTextResult(ctx, wrapped.Error()), nil
		}
		// A task-augmented call (the client supplied task params) runs the
		// run-to-completion work function so the result reaches the client
		// through tasks/result. mcp-go runs this handler in its own task
		// goroutine, so the caller is not blocked. A plain call runs Run, which
		// for an async operation returns immediately.
		if op.runResult != nil || op.mcpTaskResult != nil {
			return runMCPResultOperation(ctx, req, op, prepared), nil
		}
		return runMCPLegacyOperation(ctx, req, op, prepared), nil
	}
	return tool, handler
}

func withMCPCallerWorkingDirectory(ctx context.Context, req mcp.CallToolRequest) (context.Context, error) {
	directory, err := mcpCallerWorkingDirectory(req)
	ctx = withMCPCallerWorkingDirectoryResult(ctx, mcpCallerWorkingDirectoryResult{
		Directory: directory,
		Err:       err,
	})
	if err != nil {
		return ctx, err
	}
	return ctx, nil
}

// mcpCallerWorkingDirectory reads the client-owned working directory from the
// request metadata. The generic cwd spelling is preferred, while codex_cwd
// keeps compatibility with the Codex MCP client.
func mcpCallerWorkingDirectory(req mcp.CallToolRequest) (string, error) {
	if req.Params.Meta == nil {
		return "", fmt.Errorf("MCP caller cwd is required")
	}
	for _, name := range []string{"cwd", "codex_cwd"} {
		raw, present := req.Params.Meta.AdditionalFields[name]
		if !present {
			continue
		}
		directory, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("MCP caller %s must be a string", name)
		}
		if strings.TrimSpace(directory) == "" {
			return "", fmt.Errorf("MCP caller %s must not be empty", name)
		}
		if !filepath.IsAbs(directory) {
			return "", fmt.Errorf("MCP caller %s must be an absolute path", name)
		}
		info, statErr := os.Stat(directory)
		if statErr != nil {
			slog.Warn("mcp.caller_cwd_stat_failed", "concern", "mcp.server.context", "component", "clispec", "path", directory, "err", statErr)
			return "", fmt.Errorf("read MCP caller %s %q: %w", name, directory, statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("MCP caller %s %q is not a directory", name, directory)
		}
		return directory, nil
	}
	return "", fmt.Errorf("MCP caller cwd is required")
}

func (op Operation[I, P]) mcpToolOptions() []mcp.ToolOption {
	options := []mcp.ToolOption{mcp.WithDescription(op.mcpDescription())}
	for _, arg := range op.Args {
		properties := []mcp.PropertyOption{mcp.Description(arg.Description)}
		if arg.Required {
			properties = append(properties, mcp.Required())
		}
		options = append(options, mcp.WithString(arg.MCPName, properties...))
	}
	for _, param := range op.Params {
		if param.CLIOnly {
			continue
		}
		options = append(options, param.mcpOption())
	}
	if op.MCPTaskSupport != "" {
		options = append(options, mcp.WithTaskSupport(op.MCPTaskSupport))
	}
	return options
}

func bindMCPArgs[I Input](in *I, args []Arg[I], req mcp.CallToolRequest) string {
	for _, arg := range args {
		missing, ok := bindMCPArg(in, arg, req)
		if !ok {
			return missing
		}
	}
	return ""
}

func bindMCPArg[I Input](in *I, arg Arg[I], req mcp.CallToolRequest) (string, bool) {
	if arg.Required {
		value, err := req.RequireString(arg.MCPName)
		if err != nil {
			return arg.MCPName, false
		}
		if strings.TrimSpace(value) == "" {
			return arg.MCPName, false
		}
		arg.bind(in, value)
		return "", true
	}
	value := req.GetString(arg.MCPName, "")
	if strings.TrimSpace(value) != "" {
		arg.bind(in, value)
	}
	return "", true
}

func bindMCPParams[I Input](in *I, params []Param[I], req mcp.CallToolRequest) {
	for _, param := range params {
		if param.CLIOnly {
			continue
		}
		param.decodeMCP(in, req)
	}
}

func runMCPResultOperation[I Input, P Prepared](
	ctx context.Context,
	req mcp.CallToolRequest,
	op Operation[I, P],
	prepared P,
) *mcp.CallToolResult {
	ctx = withSurface(ctx, SurfaceMCP)
	var (
		result Result
		runErr error
	)
	switch {
	case op.mcpTaskResult != nil && req.Params.Task != nil:
		result, runErr = op.mcpTaskResult(ctx, prepared)
	case op.runResult != nil:
		result, runErr = op.runResult(ctx, prepared)
	case op.mcpTaskResult != nil:
		runErr = fmt.Errorf("%s requires task-augmented MCP calls", op.Name.Canonical)
	default:
		runErr = fmt.Errorf("%s has no result execution path", op.Name.Canonical)
	}
	if runErr != nil {
		return newMCPTextResult(ctx, runErr.Error())
	}
	rendered, err := renderMCPResult(ctx, op.outputKind, result)
	if err != nil {
		return newMCPTextResult(ctx, err.Error())
	}
	return rendered
}

func runMCPLegacyOperation[I Input, P Prepared](
	ctx context.Context,
	req mcp.CallToolRequest,
	op Operation[I, P],
	prepared P,
) *mcp.CallToolResult {
	sink := &MCPSink{buf: strings.Builder{}}
	var runErr error
	if op.MCPTaskRun != nil && req.Params.Task != nil {
		runErr = op.MCPTaskRun(ctx, prepared, sink)
	} else {
		runErr = op.Run(ctx, prepared, SurfaceMCP, sink)
	}
	text := sink.String()
	if runErr != nil {
		text = runErr.Error()
	}
	return newMCPTextResult(ctx, text)
}

func newMCPTextResult(ctx context.Context, text string) *mcp.CallToolResult {
	return mcp.NewToolResultText(response.Text(ctx, text))
}

// mcpOption builds the schema property for one parameter.
func (p Param[I]) mcpOption() mcp.ToolOption {
	switch p.Kind {
	case KindEnum:
		properties := []mcp.PropertyOption{mcp.Description(p.Description), mcp.Enum(p.Values...)}
		if p.DefaultStr != "" {
			properties = append(properties, mcp.DefaultString(p.DefaultStr))
		}
		return mcp.WithString(p.Canonical, properties...)
	case KindInt:
		properties := []mcp.PropertyOption{mcp.Description(p.Description)}
		if p.DefaultInt != 0 {
			properties = append(properties, mcp.DefaultNumber(float64(p.DefaultInt)))
		}
		return mcp.WithNumber(p.Canonical, properties...)
	case KindBool:
		properties := []mcp.PropertyOption{mcp.Description(p.Description), mcp.DefaultBool(p.DefaultBool)}
		return mcp.WithBoolean(p.Canonical, properties...)
	case KindFloat:
		properties := []mcp.PropertyOption{mcp.Description(p.Description)}
		if p.DefaultFloat != 0 {
			properties = append(properties, mcp.DefaultNumber(p.DefaultFloat))
		}
		return mcp.WithNumber(p.Canonical, properties...)
	case KindStringList:
		properties := []mcp.PropertyOption{mcp.Description(p.Description)}
		return mcp.WithArray(p.Canonical, properties...)
	case KindEnumList:
		properties := []mcp.PropertyOption{mcp.Description(p.Description), mcp.WithStringEnumItems(p.Values)}
		if p.Required {
			properties = append(properties, mcp.Required())
		}
		return mcp.WithArray(p.Canonical, properties...)
	case KindString:
		fallthrough
	default:
		properties := []mcp.PropertyOption{mcp.Description(p.Description)}
		if p.Required {
			properties = append(properties, mcp.Required())
		}
		if p.DefaultStr != "" {
			properties = append(properties, mcp.DefaultString(p.DefaultStr))
		}
		return mcp.WithString(p.Canonical, properties...)
	}
}

// decodeMCP reads one parameter from the request into the input struct. An
// enum value outside the allowed list falls back to the default.
func (p Param[I]) decodeMCP(in *I, req mcp.CallToolRequest) {
	switch p.Kind {
	case KindEnum:
		raw := strings.TrimSpace(req.GetString(p.Canonical, p.DefaultStr))
		if !enumContains(p.Values, raw) {
			raw = p.DefaultStr
		}
		p.bindString(in, raw)
	case KindInt:
		p.bindInt(in, req.GetInt(p.Canonical, p.DefaultInt))
	case KindBool:
		p.bindBool(in, req.GetBool(p.Canonical, p.DefaultBool))
	case KindFloat:
		p.bindFloat(in, req.GetFloat(p.Canonical, p.DefaultFloat))
	case KindStringList:
		p.bindStrSlice(in, req.GetStringSlice(p.Canonical, p.DefaultStrSlice))
	case KindEnumList:
		raw := req.GetStringSlice(p.Canonical, nil)
		valid := make([]string, 0, len(raw))
		for _, item := range raw {
			candidate := strings.TrimSpace(item)
			if enumContains(p.Values, candidate) {
				valid = append(valid, candidate)
			}
		}
		p.bindStrSlice(in, valid)
	case KindString:
		fallthrough
	default:
		p.bindString(in, req.GetString(p.Canonical, p.DefaultStr))
	}
}
