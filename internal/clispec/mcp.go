package clispec

import (
	"context"
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
func (op Operation[I]) mcpDescription() string {
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
func (op Operation[I]) mcpTool() (mcp.Tool, server.ToolHandlerFunc) {
	options := []mcp.ToolOption{mcp.WithDescription(op.mcpDescription())}
	for _, arg := range op.Args {
		properties := []mcp.PropertyOption{mcp.Required(), mcp.Description(arg.Description)}
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
	tool := mcp.NewTool(op.Name.MCP(), options...)

	handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in := op.New()
		missingArg := ""
		for _, arg := range op.Args {
			value, err := req.RequireString(arg.MCPName)
			if err != nil {
				missingArg = arg.MCPName
				break
			}
			arg.bind(&in, value)
		}
		if missingArg != "" {
			return newMCPTextResult(ctx, missingArg+" is required"), nil
		}
		for _, param := range op.Params {
			if param.CLIOnly {
				continue
			}
			param.decodeMCP(&in, req)
		}
		sink := &MCPSink{buf: strings.Builder{}}
		// A task-augmented call (the client supplied task params) runs the
		// run-to-completion work function so the result reaches the client
		// through tasks/result. mcp-go runs this handler in its own task
		// goroutine, so the caller is not blocked. A plain call runs Run, which
		// for an async operation returns immediately.
		var runErr error
		if op.MCPTaskRun != nil && req.Params.Task != nil {
			runErr = op.MCPTaskRun(ctx, in, sink)
		} else {
			runErr = op.Run(ctx, in, SurfaceMCP, sink)
		}
		text := sink.String()
		if runErr != nil {
			text = runErr.Error()
		}
		return newMCPTextResult(ctx, text), nil
	}
	return tool, handler
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
