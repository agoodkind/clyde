// Package mcpserver exposes Clyde transcript tools as an MCP stdio server.
package mcpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"goodkind.io/clyde/internal/audit"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
	"goodkind.io/gklog/trace"
)

//go:embed getting_started.md
var gettingStartedPrompt string

const mcpRequestIDHeader = "X-Clyde-Mcp-Request-Id"

// maxConcurrentMCPTasks bounds how many task-augmented tool calls run at once.
const maxConcurrentMCPTasks = 4

// Server is the top-level holder for the MCP stdio server.
type Server struct {
	// Tunnels tracks every in-flight MCP tool call.
	Tunnels *livetrack.Registry[MCPMeta]

	log     *slog.Logger
	cleanup func()
}

// NewServer constructs a Server with an initialized livetrack registry.
func NewServer(auditRotation audit.RotationConfig) *Server {
	MCPMeta{
		ServerName: "",
		Method:     "",
		RequestID:  "",
		Tool:       "",
		Op:         "",
	}.IsLivetrackMeta()
	log, cleanup := audit.NewLogger("mcp", auditRotation)
	registry := livetrack.New[MCPMeta](livetrack.Options[MCPMeta]{
		Component:     "mcpserver",
		Concern:       slogger.ConcernMCPServerRequest,
		Log:           log,
		PollEvery:     50 * time.Millisecond,
		CloserGrace:   2 * time.Second,
		ParallelClose: false,
		Now:           nil,
	})
	return &Server{
		Tunnels: registry,
		log:     log,
		cleanup: cleanup,
	}
}

// Serve starts the MCP stdio server and blocks until the client disconnects.
func (srv *Server) Serve(ctx context.Context) error {
	defer srv.cleanup()
	slog.SetDefault(srv.log)

	serveMeta := MCPMeta{
		ServerName: "clyde",
		Method:     "",
		RequestID:  "",
		Tool:       "",
		Op:         "serve",
	}
	srv.log.InfoContext(ctx, "mcp.server.starting", "concern", "mcp.server.requests", "server_name", serveMeta.ServerName, "op", serveMeta.Op)
	serveHandle, err := srv.Tunnels.Register(ctx, "mcp.serve", serveMeta, &contextCancelCloser{cancel: func() {}})
	if err == nil {
		defer srv.Tunnels.Release(ctx, serveHandle, "mcp.serve.done")
	}

	mcpServer := server.NewMCPServer("clyde", "0.13.0-dev",
		server.WithHooks(newHooks()),
		// Enable the 2025-11-25 Tasks primitive (list, cancel, tool-call tasks)
		// so a Tasks-capable client can run a task-augmented search_conversation
		// and poll tasks/get, tasks/list, tasks/result, and tasks/cancel. The
		// task result carries the rendered search output, including the
		// result_id, which also resolves through the bespoke search_status and
		// analyze tools.
		//
		// Two mark3labs/mcp-go v0.54.1 quirks are worked around. (1) Its server
		// returns a tool result's content at the top level of the tasks/result
		// response, but its own client parser (mcp.ParseTaskResultResult) reads
		// it from result.content, so mcp-go-based clients see empty content even
		// though the wire response is correct; non-mcp-go clients are unaffected.
		// (2) It cancels the task context as soon as the task handle is returned,
		// so the run-to-completion path in daemon.SearchToCompletion detaches its
		// daemon calls with context.WithoutCancel to let the search finish.
		server.WithTaskCapabilities(true, true, true),
		server.WithMaxConcurrentTasks(maxConcurrentMCPTasks),
	)
	mcpServer.Use(toolCallMiddleware(srv.Tunnels, "clyde"))
	registerPrompt(mcpServer)
	registerTools(mcpServer)
	return serveStdioLocked(ctx, mcpServer, srv.Tunnels, os.Stdin, os.Stdout)
}

func newHooks() *server.Hooks {
	hooks := &server.Hooks{}
	if hook := newRequestIDHook(); hook != nil {
		hooks.AddBeforeCallTool(hook)
	}
	return hooks
}

func newRequestIDHook() server.OnBeforeCallToolFunc {
	hookType := reflect.TypeFor[server.OnBeforeCallToolFunc]()
	hookValue := reflect.MakeFunc(hookType, func(arguments []reflect.Value) []reflect.Value {
		if len(arguments) != 3 {
			return nil
		}
		request, ok := arguments[2].Interface().(*mcp.CallToolRequest)
		if !ok {
			return nil
		}
		stampCallToolRequestID(formatJSONRPCID(arguments[1]), request)
		return nil
	})
	hook, ok := hookValue.Interface().(server.OnBeforeCallToolFunc)
	if !ok {
		return nil
	}
	return hook
}

func stampCallToolRequestID(requestID string, request *mcp.CallToolRequest) {
	if request == nil {
		return
	}
	if requestID == "" {
		return
	}
	if request.Header == nil {
		request.Header = http.Header{}
	}
	request.Header.Set(mcpRequestIDHeader, requestID)
}

func formatJSONRPCID(value reflect.Value) string {
	if !value.IsValid() {
		return ""
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.CanInterface() {
		if requestID, ok := value.Interface().(mcp.RequestId); ok {
			return formatJSONRPCID(reflect.ValueOf(requestID.Value()))
		}
		if number, ok := value.Interface().(json.Number); ok {
			return number.String()
		}
	}
	switch value.Kind() {
	case reflect.Invalid:
		return ""
	case reflect.Bool:
		return strconv.FormatBool(value.Bool())
	case reflect.String:
		return strings.TrimSpace(value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		floatValue := value.Float()
		if floatValue == float64(int64(floatValue)) {
			return strconv.FormatInt(int64(floatValue), 10)
		}
		return strconv.FormatFloat(floatValue, 'f', -1, 64)
	case reflect.Complex64, reflect.Complex128:
		if !value.CanInterface() {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(value.Interface()))
	case reflect.Array, reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.Struct, reflect.UnsafePointer:
		if !value.CanInterface() {
			return ""
		}
		return strings.TrimSpace(fmt.Sprint(value.Interface()))
	}
	return ""
}

func registerPrompt(mcpServer *server.MCPServer) {
	mcpServer.AddPrompt(
		mcp.Prompt{
			Name:        "clyde",
			Description: "Get started with Clyde transcript search and export.",
		},
		func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			_ = ctx
			_ = req
			return &mcp.GetPromptResult{
				Description: "Clyde transcript tools",
				Messages: []mcp.PromptMessage{{
					Role:    mcp.RoleUser,
					Content: mcp.NewTextContent(gettingStartedPrompt),
				}},
			}, nil
		},
	)
}

func registerTools(mcpServer *server.MCPServer) {
	clispec.RenderMCP(clispec.NewConversationRegistry(), mcpServer)
}

func serveStdioLocked(parent context.Context, mcpSrv *server.MCPServer, reg *livetrack.Registry[MCPMeta], stdin io.Reader, stdout io.Writer) error {
	stdio := server.NewStdioServer(mcpSrv)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigChan)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(ctx, "mcp.stdio.signal_watcher_panic", "concern", "mcp.server.requests", "component", "mcpserver", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		select {
		case <-sigChan:
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	listenErr := stdio.Listen(ctx, stdin, newMCPStdoutWriter(stdout))
	drainCtx, drainCancel := context.WithTimeout(parent, 5*time.Second)
	defer drainCancel()
	result := reg.Drain(drainCtx, "mcp.shutdown")
	if result.ForceClosed > 0 {
		slog.WarnContext(parent, "mcp.stdio.handlers_force_closed", "concern", "mcp.server.requests", "component", "mcpserver",
			"force_closed", result.ForceClosed,
			"duration_ms", result.Duration.Milliseconds(),
		)
	}
	if listenErr != nil {
		slog.WarnContext(parent, "mcp.stdio.listen_failed", "concern", "mcp.server.requests", "component", "mcpserver", "err", listenErr)
		return fmt.Errorf("mcpserver: stdio listen: %w", listenErr)
	}
	return nil
}

func toolCallMiddleware(reg *livetrack.Registry[MCPMeta], serverName string) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
			requestID := strings.TrimSpace(req.Header.Get(mcpRequestIDHeader))
			handlerCtx, corr := correlation.Ensure(ctx, requestID)
			defer trace.Op(handlerCtx, "mcp.tool_call."+req.Params.Name)(&err)
			handlerCtx, handlerCancel := context.WithCancel(handlerCtx)
			closer := &contextCancelCloser{cancel: handlerCancel}
			handle, regErr := reg.Register(handlerCtx, "mcp.tool_call", MCPMeta{
				ServerName: serverName,
				Method:     req.Method,
				RequestID:  corr.RequestID,
				Tool:       req.Params.Name,
				Op:         "tool_call",
			}, closer)
			if regErr != nil {
				handlerCancel()
				slog.WarnContext(handlerCtx, "mcp.tool_call.registry_closed", "concern", "mcp.server.requests", "component", "mcpserver", "tool", req.Params.Name, "err", regErr)
				return nil, fmt.Errorf("mcp: server draining, tool call rejected: %w", regErr)
			}
			defer reg.Release(handlerCtx, handle, "mcp.tool_call.completed")
			result, err = next(handlerCtx, req)
			handlerCancel()
			return result, err
		}
	}
}

type contextCancelCloser struct {
	cancel context.CancelFunc
}

// Close cancels the associated context. It satisfies livetrack.Closer.
func (c *contextCancelCloser) Close(_ string) error {
	c.cancel()
	return nil
}
