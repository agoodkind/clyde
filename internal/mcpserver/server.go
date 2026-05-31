// Package mcpserver exposes Clyde transcript tools as an MCP stdio server.
package mcpserver

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"goodkind.io/clyde/internal/audit"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/trace"
)

//go:embed getting_started.md
var gettingStartedPrompt string

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

	mcpServer := server.NewMCPServer("clyde", "0.13.0-dev")
	mcpServer.Use(toolCallMiddleware(srv.Tunnels, "clyde"))
	registerPrompt(mcpServer)
	registerTools(mcpServer)
	return serveStdioLocked(ctx, mcpServer, srv.Tunnels, os.Stdin, os.Stdout)
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
			defer trace.Op(ctx, "mcp.tool_call."+req.Params.Name)(&err)
			handlerCtx, handlerCancel := context.WithCancel(ctx)
			closer := &contextCancelCloser{cancel: handlerCancel}
			handle, regErr := reg.Register(handlerCtx, "mcp.tool_call", MCPMeta{
				ServerName: serverName,
				Method:     req.Method,
				RequestID:  "",
				Tool:       req.Params.Name,
				Op:         "tool_call",
			}, closer)
			if regErr != nil {
				handlerCancel()
				slog.WarnContext(ctx, "mcp.tool_call.registry_closed", "concern", "mcp.server.requests", "component", "mcpserver", "tool", req.Params.Name, "err", regErr)
				return nil, fmt.Errorf("mcp: server draining, tool call rejected: %w", regErr)
			}
			defer reg.Release(ctx, handle, "mcp.tool_call.completed")
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
