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

// ListConversations asks the running daemon for its conversation index and
// returns the typed records. When the daemon is not reachable it returns a
// clear error that names the rpc socket and points at `clyde daemon status`.
func ListConversations(ctx context.Context) ([]conversation.Record, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ListConversations(rpcCtx, &clydev1.ListConversationsRequest{})
	if err != nil {
		return nil, daemonRPCError(rpcCtx, "list conversations", err)
	}

	wireRecords := resp.GetConversations()
	records := make([]conversation.Record, 0, len(wireRecords))
	for _, wire := range wireRecords {
		records = append(records, conversation.Record{
			ID:            wire.GetId(),
			Provider:      providerFromProto(wire.GetProvider()),
			NativeID:      wire.GetNativeId(),
			Title:         wire.GetTitle(),
			WorkspaceRoot: wire.GetWorkspaceRoot(),
			ArtifactPath:  wire.GetArtifactPath(),
			ArtifactKind:  wire.GetArtifactKind(),
			Model:         wire.GetModel(),
			CreatedAt:     time.Unix(wire.GetCreatedAtUnix(), 0),
			UpdatedAt:     time.Unix(wire.GetUpdatedAtUnix(), 0),
			SizeBytes:     wire.GetSizeBytes(),
			Archived:      wire.GetArchived(),
		})
	}
	return records, nil
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

// searchTaskPollInterval is how often the run-to-completion path polls the
// daemon for a terminal search status.
const searchTaskPollInterval = 500 * time.Millisecond

// SearchToCompletion starts a search and blocks until it reaches a terminal
// state, returning the rendered result. It is the run-to-completion path used by
// the native MCP task handler; mcp-go runs it in a task goroutine so the client
// is not blocked and can poll tasks/get. Canceling ctx (via tasks/cancel)
// cancels the underlying search.
func SearchToCompletion(ctx context.Context, conversationID, query, depth string) (string, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.conn.Close() }()

	startCtx, startCancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	startResp, err := client.rpc.SearchConversation(startCtx, &clydev1.SearchConversationRequest{
		ConversationId: conversationID,
		Query:          query,
		Depth:          depth,
	})
	startCancel()
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_to_completion.start_failed", "concern", "mcp.server.context", "component", "daemon", "err", err)
		return "", daemonRPCError(ctx, "start search", err)
	}
	resultID := startResp.GetResultId()
	if resultID == "" {
		return startResp.GetText(), nil
	}

	ticker := time.NewTicker(searchTaskPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancelCtx, cancelCancel := context.WithTimeout(context.WithoutCancel(ctx), analysisClientRPCTimeout)
			_, _ = client.rpc.CancelSearch(cancelCtx, &clydev1.CancelSearchRequest{ResultId: resultID})
			cancelCancel()
			return "", fmt.Errorf("search task canceled")
		case <-ticker.C:
			statusCtx, statusCancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
			resp, err := client.rpc.GetSearchStatus(statusCtx, &clydev1.GetSearchStatusRequest{ResultId: resultID})
			statusCancel()
			if err != nil {
				slog.WarnContext(ctx, "daemon.search_to_completion.poll_failed", "concern", "mcp.server.context", "component", "daemon", "result_id", resultID, "err", err)
				return "", daemonRPCError(ctx, "poll search status", err)
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
	}
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
