package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// controlMaxMessageBytes is the message-size ceiling for the control leg, set on
// both the daemon server and the CLI/MCP client. Large reads and exports cross
// the wire as chunk streams, so this is a belt-and-suspenders bound for any
// remaining unary control RPC rather than the primary mechanism.
const controlMaxMessageBytes = 128 << 20

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

// RebindDaemon asks the running worker to replace itself with one that binds
// its config-driven listeners fresh, for a config edit that moves a listener
// address. It takes the same daemon.reload.lock flock as ReloadDaemon so a
// watcher-driven rebind serializes against any CLI-driven reload.
func RebindDaemon(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
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
	resp, err := client.rpc.RebindDaemon(rpcCtx, &clydev1.ReloadDaemonRequest{})
	if err != nil {
		slog.WarnContext(rpcCtx, "daemon.client.rebind.rpc_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("daemon rebind rpc: %w", err)
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
	stream, err := client.rpc.StreamConversation(rpcCtx, &clydev1.GetConversationRequest{
		ConversationId: conversationID,
		LastN:          int64(lastN),
	})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation", err)
	}
	text, err := reassembleConversationChunks(rpcCtx, stream)
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation", err)
	}
	return text, nil
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
	stream, err := client.rpc.StreamConversationContext(rpcCtx, &clydev1.GetConversationContextRequest{
		ConversationId: conversationID,
		Timestamp:      timestamp,
		MessageIndex:   int64(messageIndex),
		Before:         int64(before),
		After:          int64(after),
	})
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation context", err)
	}
	text, err := reassembleConversationChunks(rpcCtx, stream)
	if err != nil {
		return "", daemonRPCError(rpcCtx, "get conversation context", err)
	}
	return text, nil
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
		Query:                options.Query,
		Limit:                int64(options.Limit),
		Provider:             protoProvider(options.Provider),
		Workspace:            options.WorkspaceRoot,
		IncludeArchived:      options.IncludeArchived,
		Roles:                options.Roles,
		FromUnix:             options.FromUnix,
		UntilUnix:            options.UntilUnix,
		MinScore:             options.MinScore,
		PerConversationLimit: int64(options.PerConversationLimit),
		ConversationId:       options.ConversationID,
		ContextWindow:        int64(options.ContextWindow),
	})
	if err != nil {
		return conversation.SearchConversationsResult{}, daemonRPCError(rpcCtx, "search conversations", err)
	}
	source := searchSourceFromProto(resp.GetSource())
	matches := make([]conversation.SearchMatch, 0, len(resp.GetMatches()))
	for _, wire := range resp.GetMatches() {
		record := conversationRecordFromProto(wire.GetConversation())
		matches = append(matches, conversation.SearchMatch{
			Record:        record,
			MessageIndex:  int(wire.GetMessageIndex()),
			Role:          wire.GetRole(),
			Timestamp:     time.Unix(wire.GetTimestampUnix(), 0),
			Snippet:       wire.GetSnippet(),
			Score:         wire.GetScore(),
			ContextWindow: wire.GetContextWindow(),
		})
	}
	return conversation.SearchConversationsResult{
		Matches:              matches,
		ConversationsScanned: int(resp.GetConversationsScanned()),
		ReturnedCount:        int(resp.GetReturnedCount()),
		Limit:                int(resp.GetLimit()),
		HasMore:              resp.GetHasMore(),
		Source:               source,
		Outcome:              conversation.SearchResultOutcomeFromSource(source),
		Facets:               searchFacetsFromProto(resp.GetFacets()),
		Freshness:            searchFreshnessFromProto(resp.GetFreshness()),
		FilterAccounting:     filterAccountingFromProto(resp.GetFilterAccounting()),
	}, nil
}

// searchSourceFromProto maps the wire search source onto its domain enum.
func searchSourceFromProto(source clydev1.SearchSource) conversation.SearchSource {
	switch source {
	case clydev1.SearchSource_SEARCH_SOURCE_SEMANTIC:
		return conversation.SearchSourceSemantic
	case clydev1.SearchSource_SEARCH_SOURCE_LITERAL:
		return conversation.SearchSourceLiteral
	case clydev1.SearchSource_SEARCH_SOURCE_LITERAL_DISABLED_COLD:
		return conversation.SearchSourceLiteralDisabledCold
	case clydev1.SearchSource_SEARCH_SOURCE_UNSPECIFIED:
		return conversation.SearchSourceUnspecified
	default:
		return conversation.SearchSourceUnspecified
	}
}

// searchFacetsFromProto maps the wire facets onto the domain facets. A nil
// message yields empty facet slices.
func searchFacetsFromProto(facets *clydev1.SearchFacets) conversation.SearchFacets {
	return conversation.SearchFacets{
		Workspaces: facetCountsFromProto(facets.GetWorkspaces()),
		Providers:  facetCountsFromProto(facets.GetProviders()),
		Models:     facetCountsFromProto(facets.GetModels()),
	}
}

// facetCountsFromProto maps one wire facet dimension onto its domain counts.
func facetCountsFromProto(counts []*clydev1.SearchFacetCount) []conversation.SearchFacetCount {
	if len(counts) == 0 {
		return nil
	}
	out := make([]conversation.SearchFacetCount, 0, len(counts))
	for _, count := range counts {
		out = append(out, conversation.SearchFacetCount{Value: count.GetValue(), Count: int(count.GetCount())})
	}
	return out
}

// searchFreshnessFromProto maps the wire freshness onto the domain form. A nil
// message yields the zero value.
func searchFreshnessFromProto(freshness *clydev1.SearchFreshness) conversation.SearchFreshness {
	return conversation.SearchFreshness{
		Manifest:     int(freshness.GetManifest()),
		Needed:       int(freshness.GetNeeded()),
		Embedded:     int(freshness.GetEmbedded()),
		Pending:      int(freshness.GetPending()),
		LastSyncUnix: freshness.GetLastSyncUnix(),
	}
}

// filterAccountingFromProto maps the wire filter funnel onto the domain stages.
func filterAccountingFromProto(accounting *clydev1.FilterAccounting) []conversation.FilterStage {
	stages := accounting.GetStages()
	if len(stages) == 0 {
		return nil
	}
	out := make([]conversation.FilterStage, 0, len(stages))
	for _, stage := range stages {
		out = append(out, conversation.FilterStage{Name: stage.GetName(), Remaining: int(stage.GetRemaining())})
	}
	return out
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
	stream, err := client.rpc.StreamExportTranscript(rpcCtx, exportTranscriptRequest(conversationID, options))
	if err != nil {
		return nil, daemonRPCError(rpcCtx, "export transcript", err)
	}
	body, err := reassembleExportChunks(rpcCtx, stream)
	if err != nil {
		return nil, daemonRPCError(rpcCtx, "export transcript", err)
	}
	return body, nil
}

// reassembleConversationChunks reads a ConversationChunk stream to completion and
// returns the concatenated text. Chunk boundaries carry no semantics, so it
// stitches the raw bytes and converts once at the end, which keeps a boundary
// that falls inside a multibyte rune intact.
func reassembleConversationChunks(ctx context.Context, stream grpc.ServerStreamingClient[clydev1.ConversationChunk]) (string, error) {
	var body []byte
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return string(body), nil
		}
		if err != nil {
			slog.WarnContext(ctx, "daemon.client.conversation_chunk.recv_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", err,
			)
			return "", fmt.Errorf("receive conversation chunk: %w", err)
		}
		body = append(body, chunk.GetText()...)
	}
}

// reassembleExportChunks reads an ExportChunk stream to completion and returns
// the concatenated body bytes.
func reassembleExportChunks(ctx context.Context, stream grpc.ServerStreamingClient[clydev1.ExportChunk]) ([]byte, error) {
	var body []byte
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return body, nil
		}
		if err != nil {
			slog.WarnContext(ctx, "daemon.client.export_chunk.recv_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", err,
			)
			return nil, fmt.Errorf("receive export chunk: %w", err)
		}
		body = append(body, chunk.GetBody()...)
	}
}

func exportTranscriptRequest(conversationID string, options conversation.ExportOptions) *clydev1.ExportTranscriptRequest {
	return &clydev1.ExportTranscriptRequest{
		ConversationId:         conversationID,
		Format:                 string(options.Format),
		Whitespace:             string(options.Whitespace),
		HistoryStart:           int64(options.HistoryStart),
		IncludeSystemPrompts:   options.Content.Has(conversation.ContentKindSystemPrompts),
		IncludeSystemMessages:  options.Content.Has(conversation.ContentKindSystemMessages),
		IncludeToolOutputs:     options.Content.Has(conversation.ContentKindToolOutputs),
		IncludeRawJsonMetadata: options.Content.Has(conversation.ContentKindRawJSONMetadata),
		IncludeThinking:        options.Content.Has(conversation.ContentKindThinking),
		IncludeToolSummaries:   options.Content.Has(conversation.ContentKindToolSummaries),
		IncludeToolCalls:       options.Content.Has(conversation.ContentKindToolCalls),
		IncludeChat:            options.Content.Has(conversation.ContentKindChat),
		CompactionScope:        string(options.Compaction.Scope),
		CompactionDetail:       string(options.Compaction.Detail),
		CompactionCheckpoint:   int64(options.Compaction.CheckpointNumber),
	}
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

// SeedBaseline asks the daemon to build a wire baseline from the capture
// store's deduped shape corpus and write it as the current baseline for the
// given upstream.
func SeedBaseline(ctx context.Context, upstream string, includeUA, excludeUA []string) (mitm.SeedBaselineResult, error) {
	empty := mitm.SeedBaselineResult{Upstream: "", Flavors: 0}
	client, err := connectDaemon(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.SeedBaseline(rpcCtx, &clydev1.SeedBaselineRequest{
		Upstream:  upstream,
		IncludeUa: includeUA,
		ExcludeUa: excludeUA,
	})
	if err != nil {
		return empty, daemonRPCError(rpcCtx, "seed baseline", err)
	}
	return mitm.SeedBaselineResult{
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
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(controlMaxMessageBytes),
			grpc.MaxCallSendMsgSize(controlMaxMessageBytes),
		),
	)
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
