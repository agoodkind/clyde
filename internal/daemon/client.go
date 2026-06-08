package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/mitm"
)

const (
	reloadClientOverallTimeout = 60 * time.Second
	reloadClientRPCTimeout     = 30 * time.Second
	queryClientRPCTimeout      = 30 * time.Second
	analysisClientRPCTimeout   = 10 * time.Minute
	daemonProbeTimeout         = 2 * time.Second
)

type daemonClient struct {
	conn *grpc.ClientConn
	rpc  clydev1.ClydeServiceClient
}

// ReloadDaemon asks the running worker to hand its live listeners to a
// supervisor-spawned replacement worker.
func ReloadDaemon(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
	unlock, err := lockDaemonReload(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	retryCtx, cancel := context.WithTimeout(ctx, reloadClientOverallTimeout)
	defer cancel()
	client, err := connectDaemon(retryCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, rpcCancel := context.WithTimeout(retryCtx, reloadClientRPCTimeout)
	defer rpcCancel()
	resp, err := client.rpc.ReloadDaemon(rpcCtx, &clydev1.ReloadDaemonRequest{})
	if err != nil {
		slog.WarnContext(rpcCtx, "daemon.client.reload.rpc_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("daemon reload rpc: %w", err)
	}
	return resp, nil
}

// ListConversations asks the running daemon for a filtered conversation page.
// When the daemon is not reachable it returns a clear error that names the rpc
// socket and points at `clyde daemon status`.
func ListConversations(ctx context.Context, options conversation.ListOptions) (conversation.ListResult, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.ListResult{}, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ListConversations(rpcCtx, &clydev1.ListConversationsRequest{
		Limit:           int64(options.Limit),
		Offset:          int64(options.Offset),
		Provider:        protoProvider(options.Provider),
		Workspace:       options.WorkspaceRoot,
		Query:           options.Query,
		IncludeArchived: options.IncludeArchived,
		All:             options.All,
	})
	if err != nil {
		return conversation.ListResult{}, daemonRPCError(rpcCtx, "list conversations", err)
	}

	records := conversationRecordsFromProto(resp.GetConversations())
	return conversation.ListResult{
		Records:       records,
		TotalMatched:  int(resp.GetTotalMatched()),
		ReturnedCount: int(resp.GetReturnedCount()),
		Offset:        int(resp.GetOffset()),
		Limit:         int(resp.GetLimit()),
		NextOffset:    int(resp.GetNextOffset()),
		HasMore:       resp.GetHasMore(),
	}, nil
}

// GetConversation asks the daemon for one conversation transcript rendered as
// plain text.
func GetConversation(ctx context.Context, conversationID string, lastN int) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetConversation(rpcCtx, &clydev1.GetConversationRequest{
		ConversationId: conversationID,
		LastN:          int64(lastN),
	})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation", err)
	}
	return resp.GetText(), nil
}

// GetConversationContext asks the daemon for the messages around a center point
// rendered as plain text.
func GetConversationContext(ctx context.Context, conversationID, timestamp string, messageIndex, before, after int) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetConversationContext(rpcCtx, &clydev1.GetConversationContextRequest{
		ConversationId: conversationID,
		Timestamp:      timestamp,
		MessageIndex:   int64(messageIndex),
		Before:         int64(before),
		After:          int64(after),
	})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation context", err)
	}
	return resp.GetText(), nil
}

// SearchConversations asks the daemon for bounded transcript text matches
// across candidate conversations.
func SearchConversations(ctx context.Context, options conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.SearchConversationsResult{}, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.SearchConversations(rpcCtx, &clydev1.SearchConversationsRequest{
		Query:           options.Query,
		Limit:           int64(options.Limit),
		Provider:        protoProvider(options.Provider),
		Workspace:       options.WorkspaceRoot,
		IncludeArchived: options.IncludeArchived,
	})
	if err != nil {
		return conversation.SearchConversationsResult{}, daemonRPCError(rpcCtx, "search conversations", err)
	}
	matches := make([]conversation.SearchMatch, 0, len(resp.GetMatches()))
	for _, wire := range resp.GetMatches() {
		record := conversationRecordFromProto(wire.GetConversation())
		matches = append(matches, conversation.SearchMatch{
			Record:       record,
			MessageIndex: int(wire.GetMessageIndex()),
			Role:         wire.GetRole(),
			Timestamp:    time.Unix(wire.GetTimestampUnix(), 0),
			Snippet:      wire.GetSnippet(),
		})
	}
	return conversation.SearchConversationsResult{
		Matches:              matches,
		ConversationsScanned: int(resp.GetConversationsScanned()),
		ReturnedCount:        int(resp.GetReturnedCount()),
		Limit:                int(resp.GetLimit()),
		HasMore:              resp.GetHasMore(),
	}, nil
}

// SearchConversation asks the daemon to search one conversation and cache the
// result set for a later analyze call.
func SearchConversation(ctx context.Context, conversationID, query, depth string) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.SearchConversation(rpcCtx, &clydev1.SearchConversationRequest{
		ConversationId: conversationID,
		Query:          query,
		Depth:          depth,
	})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "search conversation", err)
	}
	return resp.GetText(), nil
}

// GetSearchStatus asks the daemon for the state, progress, and result of an
// async search job, rendered for display.
func GetSearchStatus(ctx context.Context, resultID string) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetSearchStatus(rpcCtx, &clydev1.GetSearchStatusRequest{ResultId: resultID})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get search status", err)
	}
	return renderSearchStatus(resp), nil
}

// CancelSearch asks the daemon to cancel a running search job.
func CancelSearch(ctx context.Context, resultID string) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.CancelSearch(rpcCtx, &clydev1.CancelSearchRequest{ResultId: resultID})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "cancel search", err)
	}
	return fmt.Sprintf("result_id: %s\nstatus: %s", resultID, searchStatusLabel(resp.GetStatus())), nil
}

func renderSearchStatus(resp *clydev1.GetSearchStatusResponse) string {
	if !resp.GetFound() {
		return fmt.Sprintf("result_id %s not found", resp.GetResultId())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "result_id: %s\n", resp.GetResultId())
	fmt.Fprintf(&b, "status: %s\n", searchStatusLabel(resp.GetStatus()))
	p := resp.GetProgress()
	if p != nil && (p.GetChunksTotal() > 0 || p.GetLayerTotal() > 0) {
		fmt.Fprintf(&b, "progress: sweep %d/%d chunks, layer %d/%d (%s)\n",
			p.GetChunksDone(), p.GetChunksTotal(), p.GetLayerIndex(), p.GetLayerTotal(), p.GetLayerName())
	}
	switch resp.GetStatus() {
	case clydev1.SearchStatus_SEARCH_STATUS_COMPLETE:
		b.WriteString("\n")
		b.WriteString(resp.GetResultText())
	case clydev1.SearchStatus_SEARCH_STATUS_FAILED:
		fmt.Fprintf(&b, "error: %s\n", resp.GetError())
	case clydev1.SearchStatus_SEARCH_STATUS_UNSPECIFIED,
		clydev1.SearchStatus_SEARCH_STATUS_PENDING,
		clydev1.SearchStatus_SEARCH_STATUS_RUNNING,
		clydev1.SearchStatus_SEARCH_STATUS_CANCELED:
		// No extra body for non-terminal or canceled states.
	}
	return b.String()
}

func searchStatusLabel(s clydev1.SearchStatus) string {
	switch s {
	case clydev1.SearchStatus_SEARCH_STATUS_PENDING:
		return "pending"
	case clydev1.SearchStatus_SEARCH_STATUS_RUNNING:
		return "running"
	case clydev1.SearchStatus_SEARCH_STATUS_COMPLETE:
		return "complete"
	case clydev1.SearchStatus_SEARCH_STATUS_FAILED:
		return "failed"
	case clydev1.SearchStatus_SEARCH_STATUS_CANCELED:
		return "canceled"
	case clydev1.SearchStatus_SEARCH_STATUS_UNSPECIFIED:
		return "unspecified"
	default:
		return "unspecified"
	}
}

func conversationRecordsFromProto(wireRecords []*clydev1.ConversationRecord) []conversation.Record {
	records := make([]conversation.Record, 0, len(wireRecords))
	for _, wire := range wireRecords {
		records = append(records, conversationRecordFromProto(wire))
	}
	return records
}

func conversationRecordFromProto(wire *clydev1.ConversationRecord) conversation.Record {
	var lineage *conversation.Lineage
	if wireLineage := wire.GetLineage(); wireLineage != nil {
		lineage = &conversation.Lineage{
			Kind:              conversation.LineageKind(wireLineage.GetKind()),
			ParentProvider:    providerFromProto(wireLineage.GetParentProvider()),
			ParentNativeID:    wireLineage.GetParentNativeId(),
			ParentMessageUUID: wireLineage.GetParentMessageUuid(),
		}
	}
	return conversation.Record{
		ID:            wire.GetId(),
		Provider:      providerFromProto(wire.GetProvider()),
		NativeID:      wire.GetNativeId(),
		Lineage:       lineage,
		Title:         wire.GetTitle(),
		WorkspaceRoot: wire.GetWorkspaceRoot(),
		ArtifactPath:  wire.GetArtifactPath(),
		ArtifactKind:  wire.GetArtifactKind(),
		Model:         wire.GetModel(),
		CreatedAt:     time.Unix(wire.GetCreatedAtUnix(), 0),
		UpdatedAt:     time.Unix(wire.GetUpdatedAtUnix(), 0),
		SizeBytes:     wire.GetSizeBytes(),
		Archived:      wire.GetArchived(),
	}
}

// searchTaskPollInterval is how often the run-to-completion path polls the
// daemon for a terminal search status.
const searchTaskPollInterval = 500 * time.Millisecond

// maxSearchTaskPolls bounds the run-to-completion poll loop so a stuck daemon
// job cannot pin a task goroutine forever. At the 500ms interval this is about
// 30 minutes, comfortably past even an extra-deep search.
const maxSearchTaskPolls = 3600

// SearchToCompletion starts a search and blocks until it reaches a terminal
// state, returning the rendered result. It is the run-to-completion path used by
// the native MCP task handler; mcp-go runs it in a task goroutine so the client
// is not blocked and can poll tasks/get. Canceling ctx (via tasks/cancel)
// cancels the underlying search.
func SearchToCompletion(ctx context.Context, conversationID, query, depth string) (string, error) {
	// mcp-go v0.54.1 ties the task goroutine's context to the create-task
	// request, which it cancels once the task handle is returned to the client.
	// Detach the daemon operations from that context so the search runs to
	// completion; the original ctx is still watched below so a real tasks/cancel
	// (which cancels the task context) stops the underlying search.
	opCtx := context.WithoutCancel(ctx)
	client, err := connectDaemon(opCtx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	startCtx, startCancel := context.WithTimeout(opCtx, analysisClientRPCTimeout)
	startResp, err := client.rpc.SearchConversation(startCtx, &clydev1.SearchConversationRequest{
		ConversationId: conversationID,
		Query:          query,
		Depth:          depth,
	})
	startCancel()
	if err != nil {
		slog.WarnContext(opCtx, "daemon.search_to_completion.start_failed", "concern", "mcp.server.context", "component", "daemon", "err", err)
		return "", daemonRPCError(opCtx, "start search", err)
	}
	resultID := startResp.GetResultId()
	if resultID == "" {
		return startResp.GetText(), nil
	}

	// Do not select on ctx.Done() here: mcp-go cancels the task context the moment
	// the task handle is returned, so it cannot distinguish a real tasks/cancel
	// from that premature cancel. The search therefore runs to completion under
	// opCtx; a Tasks client that wants to stop a running search calls the bespoke
	// search_cancel with the result_id (which the result text carries). The poll
	// is bounded so a stuck job cannot pin the task goroutine forever.
	ticker := time.NewTicker(searchTaskPollInterval)
	defer ticker.Stop()
	for range maxSearchTaskPolls {
		<-ticker.C
		statusCtx, statusCancel := context.WithTimeout(opCtx, analysisClientRPCTimeout)
		resp, err := client.rpc.GetSearchStatus(statusCtx, &clydev1.GetSearchStatusRequest{ResultId: resultID})
		statusCancel()
		if err != nil {
			slog.WarnContext(opCtx, "daemon.search_to_completion.poll_failed", "concern", "mcp.server.context", "component", "daemon", "result_id", resultID, "err", err)
			return "", daemonRPCError(opCtx, "poll search status", err)
		}
		switch resp.GetStatus() {
		case clydev1.SearchStatus_SEARCH_STATUS_COMPLETE:
			return renderSearchStatus(resp), nil
		case clydev1.SearchStatus_SEARCH_STATUS_FAILED:
			return "", fmt.Errorf("search failed: %s", resp.GetError())
		case clydev1.SearchStatus_SEARCH_STATUS_CANCELED:
			return "", fmt.Errorf("search canceled")
		case clydev1.SearchStatus_SEARCH_STATUS_UNSPECIFIED,
			clydev1.SearchStatus_SEARCH_STATUS_PENDING,
			clydev1.SearchStatus_SEARCH_STATUS_RUNNING:
			// Still running; keep polling.
		}
	}
	return "", fmt.Errorf("search task timed out waiting for completion")
}

// AnalyzeSearchResults asks the daemon to run the analysis model over a stored
// search result set.
func AnalyzeSearchResults(ctx context.Context, resultID, prompt string) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.AnalyzeSearchResults(rpcCtx, &clydev1.AnalyzeSearchResultsRequest{
		ResultId: resultID,
		Prompt:   prompt,
	})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "analyze results", err)
	}
	return resp.GetText(), nil
}

// ExportTranscript asks the daemon to export one conversation and returns the
// exported body.
func ExportTranscript(ctx context.Context, conversationID string, options conversation.ExportOptions) ([]byte, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ExportTranscript(rpcCtx, &clydev1.ExportTranscriptRequest{
		ConversationId:         conversationID,
		Format:                 string(options.Format),
		Whitespace:             string(options.Whitespace),
		HistoryStart:           int64(options.HistoryStart),
		IncludeSystemPrompts:   options.IncludeSystemPrompts,
		IncludeSystemMessages:  options.IncludeSystemMessages,
		IncludeToolOutputs:     options.IncludeToolOutputs,
		IncludeRawJsonMetadata: options.IncludeRawJSONMetadata,
		IncludeThinking:        options.IncludeThinking,
		IncludeToolCalls:       options.IncludeToolCalls,
		IncludeChat:            options.IncludeChat,
	})
	if err != nil {
		return nil, daemonRPCError(rpcCtx, "export transcript", err)
	}
	return resp.GetBody(), nil
}

// GetMITMStatus asks the daemon for each MITM listener address, whether it is
// bound, and the CA paths.
func GetMITMStatus(ctx context.Context) (MITMStatus, error) {
	empty := MITMStatus{Listeners: nil, CACertPath: "", CAKeyPath: ""}
	client, err := connectDaemon(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetMITMStatus(rpcCtx, &clydev1.GetMITMStatusRequest{})
	if err != nil {
		return empty, daemonRPCError(rpcCtx, "get mitm status", err)
	}

	wire := resp.GetListeners()
	listeners := make([]MITMListenerStatus, 0, len(wire))
	for _, item := range wire {
		listeners = append(listeners, MITMListenerStatus{
			ID:      item.GetId(),
			Address: item.GetAddress(),
			Up:      item.GetUp(),
		})
	}
	return MITMStatus{
		Listeners:  listeners,
		CACertPath: resp.GetCaCertPath(),
		CAKeyPath:  resp.GetCaKeyPath(),
	}, nil
}

// ShowCapture asks the daemon to correlate one request id across its logs and
// the SQLite capture store and returns the rendered text or JSON document.
func ShowCapture(ctx context.Context, id string, asJSON bool) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ShowCapture(rpcCtx, &clydev1.ShowCaptureRequest{Id: id, Json: asJSON})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "show capture", err)
	}
	return resp.GetOutput(), nil
}

// SeedBaseline asks the daemon to extract a v2 wire baseline from a capture
// transcript and write it for the given upstream.
func SeedBaseline(ctx context.Context, upstream, from, output string, includeUA, excludeUA []string) (mitm.SeedBaselineResult, error) {
	empty := mitm.SeedBaselineResult{Written: "", Upstream: "", Flavors: 0}
	client, err := connectDaemon(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.SeedBaseline(rpcCtx, &clydev1.SeedBaselineRequest{
		Upstream:  upstream,
		From:      from,
		Output:    output,
		IncludeUa: includeUA,
		ExcludeUa: excludeUA,
	})
	if err != nil {
		return empty, daemonRPCError(rpcCtx, "seed baseline", err)
	}
	return mitm.SeedBaselineResult{
		Written:  resp.GetWritten(),
		Upstream: resp.GetUpstream(),
		Flavors:  int(resp.GetFlavors()),
	}, nil
}

// LogsInventory asks the daemon to build a metadata-only inventory of its log
// files and return it rendered as a table or as JSON.
func LogsInventory(ctx context.Context, stateRoot string, largestFileLimit int, deep, asJSON bool) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.LogsInventory(rpcCtx, &clydev1.LogsInventoryRequest{
		StateRoot:        stateRoot,
		LargestFileLimit: int64(largestFileLimit),
		Deep:             deep,
		Json:             asJSON,
	})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "logs inventory", err)
	}
	return resp.GetOutput(), nil
}

// daemonRPCError translates a failed control-plane rpc into a caller-facing
// error. An Unavailable code means the daemon is not running, so the message
// names the rpc socket and how to check the daemon.
func daemonRPCError(ctx context.Context, operation string, err error) error {
	socketPath := config.DaemonSocketPath()
	if status.Code(err) == codes.Unavailable {
		slog.WarnContext(ctx, "daemon.client.rpc.unavailable", "concern", "process.daemon.lifecycle", "component", "daemon",
			"operation", operation,
			"socket_path", socketPath,
			"err", err,
		)
		return fmt.Errorf("clyde daemon is not running at %s; check `clyde daemon status` (launchd starts the daemon): %w", socketPath, err)
	}
	slog.WarnContext(ctx, "daemon.client.rpc.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("daemon rpc: %w", err)
}

func probeDaemonRPC(ctx context.Context) error {
	client, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.conn.Close() }()
	probeCtx, cancel := context.WithTimeout(ctx, daemonProbeTimeout)
	defer cancel()
	if _, err := client.rpc.GetProviderStats(probeCtx, &clydev1.GetProviderStatsRequest{}); err != nil {
		slog.WarnContext(probeCtx, "daemon.client.probe.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return fmt.Errorf("daemon rpc probe: %w", err)
	}
	return nil
}

func connectDaemon(ctx context.Context) (*daemonClient, error) {
	socketPath := config.DaemonSocketPath()
	target := "unix://" + socketPath
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		slog.WarnContext(ctx, "daemon.client.connect.new_client_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("connect daemon at %s: %w", socketPath, err)
	}
	return &daemonClient{
		conn: conn,
		rpc:  clydev1.NewClydeServiceClient(conn),
	}, nil
}

func lockDaemonReload(ctx context.Context) (func(), error) {
	if err := config.EnsureRuntimeDir(); err != nil {
		return nil, fmt.Errorf("ensure runtime dir for reload lock: %w", err)
	}
	lockPath := filepath.Join(config.RuntimeDir(), "daemon.reload.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "daemon.client.reload_lock.open_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("open reload lock: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("reload lock panic: %v", recovered)
			}
		}()
		done <- syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
	}()
	select {
	case <-ctx.Done():
		_ = lockFile.Close()
		return nil, fmt.Errorf("lock daemon reload: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			_ = lockFile.Close()
			slog.WarnContext(ctx, "daemon.client.reload_lock.lock_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"lock_path", lockPath,
				"err", err,
			)
			return nil, fmt.Errorf("lock reload: %w", err)
		}
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}
