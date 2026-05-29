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
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"goodkind.io/clyde/internal/audit"
	"goodkind.io/clyde/internal/config"
	conv "goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/clyde/internal/transcript"
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
	mcpServer.AddTool(
		mcp.NewTool("clyde_list_conversations",
			mcp.WithDescription("List Claude and Codex conversations."),
		),
		handleListConversations,
	)
	mcpServer.AddTool(
		mcp.NewTool("clyde_get_conversation",
			mcp.WithDescription("Get plain text from a conversation."),
			mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation id, native id, title, or artifact path.")),
			mcp.WithNumber("last_n", mcp.Description("Only return the last N messages.")),
		),
		handleGetConversation,
	)
	mcpServer.AddTool(
		mcp.NewTool("clyde_get_context",
			mcp.WithDescription("Get messages around a point in a conversation."),
			mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation id, native id, title, or artifact path.")),
			mcp.WithString("timestamp", mcp.Description("Timestamp to center on.")),
			mcp.WithNumber("message_index", mcp.Description("0-based message index to center on.")),
			mcp.WithNumber("before", mcp.Description("Messages before the center.")),
			mcp.WithNumber("after", mcp.Description("Messages after the center.")),
		),
		handleGetContext,
	)
	mcpServer.AddTool(
		mcp.NewTool("clyde_search_conversation",
			mcp.WithDescription("Search a conversation and return a result_id for follow-up analysis."),
			mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation id, native id, title, or artifact path.")),
			mcp.WithString("query", mcp.Required(), mcp.Description("Natural language query.")),
			mcp.WithString("depth", mcp.Description("quick, normal, deep, or extra-deep.")),
		),
		handleSearchConversation,
	)
	mcpServer.AddTool(
		mcp.NewTool("clyde_analyze_results",
			mcp.WithDescription("Analyze cached results from clyde_search_conversation."),
			mcp.WithString("result_id", mcp.Required(), mcp.Description("The result_id returned by clyde_search_conversation.")),
			mcp.WithString("prompt", mcp.Required(), mcp.Description("Analysis instruction.")),
		),
		handleAnalyzeResults,
	)
	mcpServer.AddTool(
		mcp.NewTool("clyde_export_transcript",
			mcp.WithDescription("Export a conversation transcript."),
			mcp.WithString("conversation_id", mcp.Required(), mcp.Description("Conversation id, native id, title, or artifact path.")),
			mcp.WithString("format", mcp.Description("markdown, html, json, or plain_text.")),
			mcp.WithString("whitespace", mcp.Description("preserve, tidy, compact, or dense.")),
			mcp.WithNumber("history_start", mcp.Description("First message index to include.")),
			mcp.WithBoolean("include_system_prompts", mcp.Description("Include system-injected prompts.")),
			mcp.WithBoolean("include_tool_outputs", mcp.Description("Include tool result bodies.")),
		),
		handleExportTranscript,
	)
}

func handleListConversations(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	_ = req
	records, err := conv.DefaultIndex.List(ctx)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to list conversations: %v", err)), nil
	}
	if len(records) == 0 {
		return mcp.NewToolResultText("No conversations found."), nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%d conversations:\n\n", len(records))
	for _, record := range records {
		fmt.Fprintf(&out, "- %s [%s] %s", record.ID, record.Provider.String(), record.Title)
		if record.WorkspaceRoot != "" {
			fmt.Fprintf(&out, " (%s)", shortPath(record.WorkspaceRoot))
		}
		out.WriteString("\n")
	}
	return mcp.NewToolResultText(out.String()), nil
}

func handleGetConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	record, err := resolveRequestedConversation(ctx, req)
	if err != nil {
		return mcp.NewToolResultText(err.Error()), nil
	}
	messages, err := conv.LoadMessages(record, false, false)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to load conversation: %v", err)), nil
	}
	lastN := int(req.GetFloat("last_n", 0))
	if lastN > 0 && lastN < len(messages) {
		messages = messages[len(messages)-lastN:]
	}
	text := transcript.RenderPlainTextWithOptions(messages, transcript.DefaultShapeOptions())
	if text == "" {
		return mcp.NewToolResultText("No conversation messages found."), nil
	}
	return mcp.NewToolResultText(text), nil
}

func handleGetContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	record, err := resolveRequestedConversation(ctx, req)
	if err != nil {
		return mcp.NewToolResultText(err.Error()), nil
	}
	messages, err := conv.LoadMessages(record, false, false)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to load conversation: %v", err)), nil
	}
	if len(messages) == 0 {
		return mcp.NewToolResultText("No conversation messages found."), nil
	}
	before := int(req.GetFloat("before", 5))
	after := int(req.GetFloat("after", 5))
	center := -1
	if timestamp := req.GetString("timestamp", ""); timestamp != "" {
		center = findNearestMessage(messages, timestamp)
	}
	if center < 0 {
		index := int(req.GetFloat("message_index", -1))
		if index >= 0 && index < len(messages) {
			center = index
		}
	}
	if center < 0 {
		return mcp.NewToolResultText("Provide timestamp or message_index to center on."), nil
	}
	start := max(center-before, 0)
	end := min(center+after+1, len(messages))
	text := fmt.Sprintf(
		"Messages %d-%d of %d centered on %d:\n\n%s",
		start,
		end-1,
		len(messages),
		center,
		transcript.RenderPlainTextWithOptions(messages[start:end], transcript.DefaultShapeOptions()),
	)
	return mcp.NewToolResultText(text), nil
}

func handleSearchConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	record, err := resolveRequestedConversation(ctx, req)
	if err != nil {
		return mcp.NewToolResultText(err.Error()), nil
	}
	query := req.GetString("query", "")
	if query == "" {
		return mcp.NewToolResultText("query is required"), nil
	}
	cfg, _ := config.LoadGlobalOrDefault()
	output, err := conv.SearchConversation(ctx, record, query, req.GetString("depth", "quick"), cfg.Search)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Search failed: %v", err)), nil
	}
	return mcp.NewToolResultText(output.Text), nil
}

func handleAnalyzeResults(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resultID := req.GetString("result_id", "")
	prompt := req.GetString("prompt", "")
	if resultID == "" || prompt == "" {
		return mcp.NewToolResultText("result_id and prompt are required"), nil
	}
	cfg, _ := config.LoadGlobalOrDefault()
	text, err := conv.AnalyzeSearchResults(ctx, resultID, prompt, cfg.Search)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Analysis failed: %v", err)), nil
	}
	return mcp.NewToolResultText(text), nil
}

func handleExportTranscript(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	record, err := resolveRequestedConversation(ctx, req)
	if err != nil {
		return mcp.NewToolResultText(err.Error()), nil
	}
	options := conv.ExportOptions{
		Format:                 parseExportFormat(req.GetString("format", "markdown")),
		HistoryStart:           int(req.GetFloat("history_start", 0)),
		Whitespace:             parseWhitespace(req.GetString("whitespace", "preserve")),
		IncludeChat:            true,
		IncludeThinking:        true,
		IncludeSystemPrompts:   req.GetBool("include_system_prompts", false),
		IncludeToolCalls:       true,
		IncludeToolOutputs:     req.GetBool("include_tool_outputs", false),
		IncludeRawJSONMetadata: false,
	}
	body, err := conv.Export(record, options)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Export failed: %v", err)), nil
	}
	return mcp.NewToolResultText(string(body)), nil
}

func resolveRequestedConversation(ctx context.Context, req mcp.CallToolRequest) (conv.Record, error) {
	selector := req.GetString("conversation_id", "")
	if selector == "" {
		return conv.Record{}, fmt.Errorf("conversation_id is required")
	}
	record, err := conv.DefaultIndex.Resolve(ctx, selector)
	if err != nil {
		slog.WarnContext(ctx, "mcp.conversation.resolve_failed", "concern", "mcp.server.context", "component", "mcpserver", "conversation_id", selector, "err", err)
		return conv.Record{}, fmt.Errorf("resolve conversation: %w", err)
	}
	return record, nil
}

func parseExportFormat(raw string) conv.ExportFormat {
	switch conv.ExportFormat(strings.TrimSpace(raw)) {
	case conv.ExportFormatMarkdown:
		return conv.ExportFormatMarkdown
	case conv.ExportFormatHTML:
		return conv.ExportFormatHTML
	case conv.ExportFormatJSON:
		return conv.ExportFormatJSON
	case conv.ExportFormatPlainText:
		return conv.ExportFormatPlainText
	default:
		return conv.ExportFormatMarkdown
	}
}

func parseWhitespace(raw string) conv.WhitespaceMode {
	switch conv.WhitespaceMode(strings.TrimSpace(raw)) {
	case conv.WhitespacePreserve:
		return conv.WhitespacePreserve
	case conv.WhitespaceTidy:
		return conv.WhitespaceTidy
	case conv.WhitespaceCompact:
		return conv.WhitespaceCompact
	case conv.WhitespaceDense:
		return conv.WhitespaceDense
	default:
		return conv.WhitespacePreserve
	}
}

func findNearestMessage(messages []transcript.Message, rawTimestamp string) int {
	var target time.Time
	for _, layout := range []string{
		"2006-01-02 15:04",
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, rawTimestamp)
		if err == nil {
			target = parsed
			break
		}
	}
	if target.IsZero() {
		return -1
	}
	best := -1
	bestDiff := time.Duration(1<<63 - 1)
	for i, message := range messages {
		diff := message.Timestamp.Sub(target)
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			best = i
		}
	}
	return best
}

func shortPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+"/") {
		return "~/" + path[len(home)+1:]
	}
	return path
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
