// Package mcpserver exposes clyde session tools as an MCP server (stdio transport).
// Claude Code connects to this process and can search/list/view sessions as tools.
package mcpserver

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"goodkind.io/clyde/internal/audit"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/search"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/clyde/internal/util"
)

// resultCache stores search results in memory so callers can reference them by ID
// for follow-up analysis without re-running the search.
type cachedResult struct {
	SessionName string
	Messages    []transcript.Message // all matched messages, flattened
	Results     []search.Result      // original results with summaries
	CreatedAt   time.Time
}

var resultCache sync.Map // map[string]*cachedResult

// storeResult saves a result to the in-memory cache and persists it to XDG cache dir.
func storeResult(resultID string, cached *cachedResult) {
	resultCache.Store(resultID, cached)
	if err := config.EnsureSearchResultCacheDir(); err != nil {
		return
	}
	path := filepath.Join(config.SearchResultCacheDir(), resultID+".json")
	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// loadResult retrieves a result from memory or disk cache.
func loadResult(resultID string) (*cachedResult, bool) {
	if _, err := uuid.Parse(resultID); err != nil {
		return nil, false
	}
	if val, ok := resultCache.Load(resultID); ok {
		return val.(*cachedResult), true
	}
	path := filepath.Join(config.SearchResultCacheDir(), resultID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cached cachedResult
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, false
	}
	resultCache.Store(resultID, &cached)
	return &cached, true
}

//go:embed getting_started.md
var gettingStartedPrompt string

// Server is the top-level holder for the MCP stdio server. Tunnels is the
// livetrack registry that tracks every in-flight tool call. It is a public
// field so code outside this package (e.g. the CLI mcp command) can query
// the registry count and drain it on process exit, matching the
// internal/mitm.Proxy.Tunnels pattern from CLYDE-270.
type Server struct {
	// Tunnels tracks every in-flight MCP tool call. Each call is registered
	// before dispatch and released after the response is written. The
	// registry is drained on server exit via Drain so no handler outlives
	// the server context.
	Tunnels *livetrack.Registry[MCPMeta]

	log     *slog.Logger
	cleanup func()
}

// NewServer constructs a Server with an initialized livetrack registry.
// Callers must call Serve to start accepting connections. The registry is
// available immediately via Tunnels so callers outside this package can
// reference the registry before Serve blocks.
func NewServer() *Server {
	log, cleanup := audit.NewLogger("mcp")
	reg := livetrack.New[MCPMeta](livetrack.Options[MCPMeta]{
		Component:     "mcpserver",
		Concern:       slogger.ConcernMCPServerRequest,
		Log:           log,
		PollEvery:     50 * time.Millisecond,
		CloserGrace:   2 * time.Second,
		ParallelClose: false,
		Now:           nil,
	})
	return &Server{
		Tunnels: reg,
		log:     log,
		cleanup: cleanup,
	}
}

// Serve starts the MCP stdio server and blocks until the client disconnects.
// Call NewServer to construct a Server before calling Serve.
//
// The stdio writer is wrapped in mcpStdoutWriter so concurrent JSON-RPC
// frames cannot interleave on [os.Stdout]. The upstream stdio transport runs
// handleNotifications and the toolCallWorker pool on separate goroutines,
// and a few upstream paths write to the session writer without going
// through the upstream writeMu. CLYDE-57.
//
// The registry at Server.Tunnels tracks every in-flight tool call. Each call
// is registered before dispatch and released after the response is written.
// The registry is drained on server exit via Drain so no handler outlives the
// server context.
func (srv *Server) Serve(ctx context.Context) error {
	defer srv.cleanup()
	slog.SetDefault(srv.log)

	// Register a lifecycle session for the server's own startup so the
	// registry shows an active entry while Serve is running, and so that
	// MCPMeta is allocated from a directly-reachable call site outside any
	// middleware closure. This mirrors how Proxy.Tunnels is used in the
	// MITM package via direct Register calls in handleConnect and
	// registerPlainHTTP (CLYDE-270).
	serveMeta := MCPMeta{
		ServerName: "clyde",
		Method:     "",
		RequestID:  "",
		Tool:       "",
		Op:         "serve",
	}
	srv.log.InfoContext(ctx, "mcp.server.starting",
		"server_name", serveMeta.ServerName,
		"op", serveMeta.Op,
		"serve_meta", serveMeta,
	)
	serverSess, err := srv.Tunnels.Register(ctx, "mcp.serve", serveMeta, &contextCancelCloser{cancel: func() {}})
	if err == nil {
		defer srv.Tunnels.Release(ctx, serverSess, "mcp.serve.done")
	}

	s := server.NewMCPServer("clyde", "0.13.0-dev")
	s.Use(toolCallMiddleware(srv.Tunnels, "clyde"))

	// --- Prompts (slash commands) ---

	s.AddPrompt(
		mcp.Prompt{
			Name:        "clyde",
			Description: "Get started with clyde session management. Lists available tools and explains how to use them.",
		},
		func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{
				Description: "clyde session management",
				Messages: []mcp.PromptMessage{
					{
						Role:    mcp.RoleUser,
						Content: mcp.NewTextContent(gettingStartedPrompt),
					},
				},
			}, nil
		},
	)

	// --- Tools ---

	s.AddTool(
		mcp.NewTool("clyde_list_sessions",
			mcp.WithDescription("List all clyde sessions with their names, workspaces, models, and context. Use this to find sessions before searching."),
			mcp.WithBoolean("all", mcp.Description("Show all sessions across all workspaces (default: current workspace only).")),
		),
		handleListSessions,
	)

	s.AddTool(
		mcp.NewTool("clyde_get_conversation",
			mcp.WithDescription("Get the plain text conversation from a session. Returns user and assistant messages without tool call details."),
			mcp.WithString("session_name", mcp.Required(), mcp.Description("Session name to retrieve.")),
			mcp.WithNumber("last_n", mcp.Description("Only return the last N messages (default: all).")),
		),
		handleGetConversation,
	)

	s.AddTool(
		mcp.NewTool("clyde_get_context",
			mcp.WithDescription("Get messages around a specific point in a session's conversation. Use after search to expand context around a match. Provide either a timestamp or message_index to center on."),
			mcp.WithString("session_name", mcp.Required(), mcp.Description("Session name.")),
			mcp.WithString("timestamp", mcp.Description("ISO timestamp to center on (e.g. '2026-04-12 15:04'). Finds nearest message.")),
			mcp.WithNumber("message_index", mcp.Description("0-based message index to center on.")),
			mcp.WithNumber("before", mcp.Description("Number of messages to include before the center (default: 5).")),
			mcp.WithNumber("after", mcp.Description("Number of messages to include after the center (default: 5).")),
		),
		handleGetContext,
	)

	s.AddTool(
		mcp.NewTool("clyde_search_conversation",
			mcp.WithDescription("Search a session's conversation history for where a topic was discussed. Returns matching messages with context and a result_id for follow-up analysis. Always start with 'quick' (embedding only, ~20s). Escalate only when quick results are insufficient."),
			mcp.WithString("session_name", mcp.Required(), mcp.Description("Session name to search.")),
			mcp.WithString("query", mcp.Required(), mcp.Description("What to search for (natural language).")),
			mcp.WithString("depth", mcp.Description("Search depth: 'quick' (embedding only, ~20s, default), 'normal' (+ LLM sweep, ~4min), 'deep' (+ rerank, ~5min), 'extra-deep' (+ large model, 20min+, warns before running).")),
		),
		handleSearchConversation,
	)

	s.AddTool(
		mcp.NewTool("clyde_analyze_results",
			mcp.WithDescription("Run an LLM analysis pass over the results from a previous clyde_search_conversation call. Use the result_id returned by search. The LLM will synthesize, extract, or summarize based on your prompt. Avoids re-running the search."),
			mcp.WithString("result_id", mcp.Required(), mcp.Description("The result_id returned by clyde_search_conversation.")),
			mcp.WithString("prompt", mcp.Required(), mcp.Description("What to extract or analyze from the search results (e.g. 'List every frustration instance with timestamp and verbatim quote').")),
		),
		handleAnalyzeResults,
	)

	return serveStdioLocked(ctx, s, srv.Tunnels, os.Stdin, os.Stdout)
}

// serveStdioLocked mirrors server.ServeStdio but routes [os.Stdout] through
// mcpStdoutWriter so every byte written by the upstream transport, by
// notification frames, and by tool responses is serialized under one mutex.
// Signal handling matches the upstream ServeStdio path.
//
// reg is the livetrack registry for in-flight tool calls. When Listen returns
// (either cleanly or on error), serveStdioLocked drains the registry under the
// parent context so every tracked handler gets a chance to finish or is
// force-closed before the function returns. The CLYDE-57 stdout mutex is
// unaffected: it still serializes every write to the underlying writer.
func serveStdioLocked(parent context.Context, mcpSrv *server.MCPServer, reg *livetrack.Registry[MCPMeta], stdin io.Reader, stdout io.Writer) error {
	stdio := server.NewStdioServer(mcpSrv)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigChan)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.ErrorContext(ctx, "mcp.stdio.signal_watcher_panic",
					"component", "mcpserver",
					"err", fmt.Errorf("panic: %v", r),
				)
			}
		}()
		select {
		case <-sigChan:
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	locked := newMCPStdoutWriter(stdout)
	listenErr := stdio.Listen(ctx, stdin, locked)

	// Drain the registry under the parent context (not the already-canceled
	// ctx) so in-flight handlers have a bounded window to finish. The Drain
	// call logs the outcome; the caller sees only the original Listen error.
	drainCtx, drainCancel := context.WithTimeout(parent, 5*time.Second)
	defer drainCancel()
	result := reg.Drain(drainCtx, "mcp.shutdown")
	if result.ForceClosed > 0 {
		slog.WarnContext(parent, "mcp.stdio.handlers_force_closed",
			"component", "mcpserver",
			"force_closed", result.ForceClosed,
			"duration_ms", result.Duration.Milliseconds(),
		)
	}

	if listenErr != nil {
		return fmt.Errorf("mcp stdio listen: %w", listenErr)
	}
	return nil
}

// toolCallMiddleware returns a ToolHandlerMiddleware that registers each
// in-flight tool call with reg before dispatch and releases it on return.
// The Closer cancels the handler's derived context so force-close during
// Drain interrupts a wedged handler. serverName is recorded in MCPMeta
// for operator snapshots.
func toolCallMiddleware(reg *livetrack.Registry[MCPMeta], serverName string) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			handlerCtx, handlerCancel := context.WithCancel(ctx)
			closer := &contextCancelCloser{cancel: handlerCancel}
			sess, err := reg.Register(handlerCtx, "mcp.tool_call", MCPMeta{
				ServerName: serverName,
				Method:     req.Method,
				RequestID:  "",
				Tool:       req.Params.Name,
				Op:         "tool_call",
			}, closer)
			if err != nil {
				// Registry is draining; reject the call so the upstream
				// returns an MCP error to the client instead of hanging.
				handlerCancel()
				slog.WarnContext(ctx, "mcp.tool_call.registry_closed",
					"component", "mcpserver",
					"tool", req.Params.Name,
					"err", err,
				)
				return nil, fmt.Errorf("mcp: server draining, tool call rejected: %w", err)
			}
			defer reg.Release(ctx, sess, "mcp.tool_call.completed")
			result, callErr := next(handlerCtx, req)
			handlerCancel()
			return result, callErr
		}
	}
}

// contextCancelCloser implements livetrack.Closer by canceling a context.
// It is used by toolCallMiddleware so force-close during Drain cancels the
// handler's derived context, interrupting any blocking I/O the handler holds.
type contextCancelCloser struct {
	cancel context.CancelFunc
}

// Close cancels the associated context. It satisfies livetrack.Closer.
func (c *contextCancelCloser) Close(_ string) error {
	c.cancel()
	return nil
}

func handleListSessions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to open session store: %v", err)), nil
	}

	// Always list all sessions globally
	sessions, err := store.List()
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to list sessions: %v", err)), nil
	}

	if len(sessions) == 0 {
		return mcp.NewToolResultText("No sessions found."), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d sessions:\n\n", len(sessions))
	for _, sess := range sessions {
		fmt.Fprintf(&sb, "- **%s**", sess.Name)
		if sess.Metadata.WorkspaceRoot != "" {
			fmt.Fprintf(&sb, " (%s)", shortPath(sess.Metadata.WorkspaceRoot))
		}
		if sess.Metadata.Context != "" {
			fmt.Fprintf(&sb, " - %s", sess.Metadata.Context)
		}
		sb.WriteString("\n")
	}
	return mcp.NewToolResultText(sb.String()), nil
}

func handleGetContext(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("session_name", "")
	if name == "" {
		return mcp.NewToolResultText("session_name is required"), nil
	}

	messages, err := loadMessages(name)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to load conversation: %v", err)), nil
	}
	if len(messages) == 0 {
		return mcp.NewToolResultText("No conversation messages found."), nil
	}

	before := int(req.GetFloat("before", 5))
	after := int(req.GetFloat("after", 5))

	// Find center point: by timestamp or by index
	center := -1
	if ts := req.GetString("timestamp", ""); ts != "" {
		center = findNearestMessage(messages, ts)
	}
	if center < 0 {
		idx := int(req.GetFloat("message_index", -1))
		if idx >= 0 && idx < len(messages) {
			center = idx
		}
	}
	if center < 0 {
		return mcp.NewToolResultText("Provide either timestamp or message_index to center on."), nil
	}

	// Extract window
	start := center - before
	start = max(start, 0)
	end := center + after + 1
	end = min(end, len(messages))

	window := messages[start:end]
	text := fmt.Sprintf("Messages %d-%d of %d (centered on %d):\n\n%s",
		start, end-1, len(messages), center, transcript.RenderPlainTextWithOptions(window, transcript.DefaultShapeOptions()))
	return mcp.NewToolResultText(text), nil
}

// findNearestMessage finds the message closest to the given timestamp string.
func findNearestMessage(messages []transcript.Message, ts string) int {
	// Try common formats
	var target time.Time
	for _, layout := range []string{
		"2006-01-02 15:04",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, ts); err == nil {
			target = t
			break
		}
	}
	if target.IsZero() {
		return -1
	}

	best := -1
	bestDiff := time.Duration(1<<63 - 1)
	for i, m := range messages {
		diff := m.Timestamp.Sub(target)
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

func handleGetConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("session_name", "")
	if name == "" {
		return mcp.NewToolResultText("session_name is required"), nil
	}

	lastN := int(req.GetFloat("last_n", 0))

	messages, err := loadMessages(name)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to load conversation: %v", err)), nil
	}

	if lastN > 0 && lastN < len(messages) {
		messages = messages[len(messages)-lastN:]
	}

	text := transcript.RenderPlainTextWithOptions(messages, transcript.DefaultShapeOptions())
	if len(text) == 0 {
		return mcp.NewToolResultText("No conversation messages found."), nil
	}
	return mcp.NewToolResultText(text), nil
}

func handleSearchConversation(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := req.GetString("session_name", "")
	query := req.GetString("query", "")
	if name == "" || query == "" {
		return mcp.NewToolResultText("session_name and query are required"), nil
	}

	messages, err := loadMessages(name)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Failed to load conversation: %v", err)), nil
	}
	if len(messages) == 0 {
		return mcp.NewToolResultText("No conversation messages found."), nil
	}

	depth := req.GetString("depth", "quick")
	cfg, _ := config.LoadGlobalOrDefault()
	results, err := search.SearchWithDepth(ctx, messages, query, cfg.Search, depth)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Search failed: %v", err)), nil
	}

	if len(results) == 0 {
		return mcp.NewToolResultText("No matching messages found."), nil
	}

	// Store matched messages in cache so the caller can run follow-up analysis.
	resultID, err := util.GenerateUUIDE()
	if err != nil {
		mcpLog.WarnContext(ctx, "mcp.search.result_uuid_failed",
			"component", "mcpserver",
			"session", name,
			"err", err,
		)
		return mcp.NewToolResultText(fmt.Sprintf("Failed to allocate search result id: %v", err)), nil
	}
	var flatMessages []transcript.Message
	for _, r := range results {
		flatMessages = append(flatMessages, r.Messages...)
	}
	storeResult(resultID, &cachedResult{
		SessionName: name,
		Messages:    flatMessages,
		Results:     results,
		CreatedAt:   currentTime(),
	})

	// Build a UUID-to-index map so we can show global message indices.
	idxMap := make(map[string]int, len(messages))
	for i, m := range messages {
		if m.UUID != "" {
			idxMap[m.UUID] = i
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "result_id: %s (pass to clyde_analyze_results for follow-up analysis)\n\n", resultID)
	fmt.Fprintf(&sb, "Use clyde_get_context with session_name=%q and message_index=N to expand around any result.\n\n", name)
	for _, r := range results {
		if r.Summary != "" {
			fmt.Fprintf(&sb, "**Found:** %s\n\n", r.Summary)
		}
		for _, m := range r.Messages {
			idx, ok := idxMap[m.UUID]
			if !ok {
				idx = -1
			}
			ts := m.Timestamp.Format("2006-01-02 15:04")
			role := "User"
			if m.Role == "assistant" {
				role = "Assistant"
			}
			if idx >= 0 {
				fmt.Fprintf(&sb, "[#%d][%s] %s:\n", idx, ts, role)
			} else {
				fmt.Fprintf(&sb, "[%s] %s:\n", ts, role)
			}
			if m.Text != "" {
				sb.WriteString(m.Text)
				sb.WriteString("\n")
			}
			if m.HasTools {
				fmt.Fprintf(&sb, "  [used: %s]\n", strings.Join(m.ToolNames(), ", "))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("---\n\n")
	}
	return mcp.NewToolResultText(sb.String()), nil
}

func handleAnalyzeResults(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resultID := req.GetString("result_id", "")
	prompt := req.GetString("prompt", "")
	if resultID == "" || prompt == "" {
		return mcp.NewToolResultText("result_id and prompt are required"), nil
	}

	cached, ok := loadResult(resultID)
	if !ok {
		return mcp.NewToolResultText(fmt.Sprintf("result_id %q not found. It may have been from a different session or the cache file may have been deleted.", resultID)), nil
	}

	if len(cached.Messages) == 0 {
		return mcp.NewToolResultText("Cached result has no messages."), nil
	}

	// Build conversation text from all cached messages.
	var convText strings.Builder
	for _, m := range cached.Messages {
		ts := m.Timestamp.Format("2006-01-02 15:04")
		role := "User"
		if m.Role == "assistant" {
			role = "Assistant"
		}
		fmt.Fprintf(&convText, "[%s] %s:\n%s\n\n", ts, role, m.Text)
	}

	fullPrompt := fmt.Sprintf("%s\n\nCONVERSATION EXCERPTS from session %q:\n\n%s",
		prompt, cached.SessionName, convText.String())

	cfg, _ := config.LoadGlobalOrDefault()
	pipeline := cfg.Search.Local.Pipeline
	model := cfg.Search.Local.Model
	if len(pipeline) > 0 {
		model = pipeline[0].Model
	}
	if model == "" {
		model = "qwen2.5-coder-32b"
	}

	client := search.NewClientForModel(cfg.Search, model)
	resp, err := client.Complete(ctx, fullPrompt)
	if err != nil {
		return mcp.NewToolResultText(fmt.Sprintf("Analysis failed: %v", err)), nil
	}

	mcpSearchLog.Logger().Info("analyze_results complete",
		"result_id", resultID,
		"session", cached.SessionName,
		"cached_messages", len(cached.Messages),
		"response_chars", len(resp),
	)

	return mcp.NewToolResultText(resp), nil
}

// loadMessages loads all parsed messages for a session by name.
func loadMessages(name string) ([]transcript.Message, error) {
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, err
	}
	sess, err := store.Resolve(name)
	if err != nil {
		mcpLog.Warn("mcp.load_messages.resolve_failed",
			"component", "mcpserver",
			"session", name,
			"err", err,
		)
		return nil, fmt.Errorf("session resolution error: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session '%s' not found", name)
	}

	homeDir, err := util.HomeDir()
	if err != nil {
		return nil, err
	}

	root := sess.Metadata.WorkspaceRoot
	if root == "" {
		root, _ = config.FindProjectRoot()
	}
	clydeRoot := root + "/.claude/clyde"

	var paths []string
	for _, prevID := range sess.Metadata.PreviousProviderSessionIDStrings() {
		if prevID != "" {
			paths = append(paths, claudeTranscriptPath(homeDir, clydeRoot, prevID))
		}
	}
	current := sess.Metadata.ProviderTranscriptPath()
	if current == "" && sess.Metadata.ProviderSessionID() != "" {
		current = claudeTranscriptPath(homeDir, clydeRoot, sess.Metadata.ProviderSessionID())
	}
	if current != "" {
		paths = append(paths, current)
	}

	var allMessages []transcript.Message
	for _, path := range paths {
		f, openErr := os.Open(path)
		if openErr != nil {
			continue
		}
		msgs, parseErr := transcript.Parse(f)
		_ = f.Close()
		if parseErr != nil {
			continue
		}
		allMessages = append(allMessages, msgs...)
	}
	return allMessages, nil
}

func claudeTranscriptPath(homeDir, clydeRoot, sessionID string) string {
	projectRoot := clydeRoot
	projectRoot = strings.TrimSuffix(projectRoot, "/.claude/clyde")
	encoded := strings.ReplaceAll(projectRoot, "/", "-")
	encoded = strings.ReplaceAll(encoded, ".", "-")
	return homeDir + "/.claude/projects/" + encoded + "/" + sessionID + ".jsonl"
}

func shortPath(root string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return root
	}
	if root == home {
		return "~"
	}
	if strings.HasPrefix(root, home+"/") {
		return "~/" + root[len(home)+1:]
	}
	return root
}
