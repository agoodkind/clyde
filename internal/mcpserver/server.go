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
	// group owns Tunnels. The MCP server is a standalone process, so it holds
	// its own single-member lifecycle group rather than the daemon's; shutdown
	// drains it through group.Quiesce, the only drain entry point.
	group *livetrack.Group

	mcp     *server.MCPServer
	log     *slog.Logger
	cleanup func()
}

// NewServer constructs a Server with an initialized livetrack registry attached
// to the server's own lifecycle group.
func NewServer(auditRotation audit.RotationConfig) *Server {
	MCPMeta{
		ServerName: "",
		Method:     "",
		RequestID:  "",
		Tool:       "",
		Op:         "",
	}.IsLivetrackMeta()
	log, cleanup := audit.NewLogger("mcp", auditRotation)
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: log})
	registry := livetrack.Attach[MCPMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseIngress,
		QuietRelevant: true,
		CancelNoWait:  false,
	}, livetrack.Options[MCPMeta]{
		Component:     "mcpserver",
		Concern:       slogger.ConcernMCPServerRequest,
		Log:           log,
		PollEvery:     50 * time.Millisecond,
		CloserGrace:   2 * time.Second,
		ParallelClose: false,
		Now:           nil,
	})
	mcpServer := server.NewMCPServer("clyde", "0.13.0-dev",
		server.WithHooks(newHooks()),
		server.WithTaskCapabilities(true, true, true),
		server.WithMaxConcurrentTasks(maxConcurrentMCPTasks),
	)
	mcpServer.Use(toolCallMiddleware(registry, "clyde"))
	registerPrompt(mcpServer)
	return &Server{
		Tunnels: registry,
		group:   group,
		mcp:     mcpServer,
		log:     log,
		cleanup: cleanup,
	}
}

// MCPServer exposes the underlying MCP server so callers can register tools.
func (srv *Server) MCPServer() *server.MCPServer {
	return srv.mcp
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

	return serveStdioLocked(ctx, srv.mcp, srv.group, mcpShutdownCap, os.Stdin, os.Stdout)
}

// mcpShutdownCap bounds how long the MCP server drains in-flight tool calls
// before force-closing them on exit. It matches the previous 5s drain deadline.
const mcpShutdownCap = 5 * time.Second

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

func serveStdioLocked(parent context.Context, mcpSrv *server.MCPServer, group *livetrack.Group, shutdownCap time.Duration, stdin io.Reader, stdout io.Writer) error {
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
	// Drain the in-flight MCP tool calls through the server's lifecycle group
	// under shutdownCap. The registry emits its own drain-complete event with the
	// force-closed count, so no bespoke force-close log is needed here.
	group.Quiesce(parent, "mcp.shutdown", livetrack.Budget{Cap: shutdownCap, IdleGrace: 0})
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
			handlerCtx, corr := newToolCallContext(ctx, requestID)
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

// newToolCallContext starts a fresh correlation for one MCP tool call. MCP stdio
// carries no inbound trace, and the serve process seeds one shared correlation
// on the base context that mcp-go derives every handler context from, so using
// correlation.Ensure here would reuse that one trace and span across every call.
// Minting a fresh correlation per call gives each tool call its own trace and
// span while keeping the per-call request id.
func newToolCallContext(ctx context.Context, requestID string) (context.Context, correlation.Context) {
	corr := correlation.New(requestID)
	return correlation.WithContext(ctx, corr), corr
}

type contextCancelCloser struct {
	cancel context.CancelFunc
}

// Close cancels the associated context. It satisfies livetrack.Closer.
func (c *contextCancelCloser) Close(_ string) error {
	c.cancel()
	return nil
}
