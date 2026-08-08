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
	"goodkind.io/clyde/internal/loginventory"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitmshow"
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

// GetConversationInfo asks the daemon for one conversation's static metadata
// and compaction segment stack.
func GetConversationInfo(ctx context.Context, conversationID string) (conversation.Info, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.Info{}, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.GetConversationInfo(rpcCtx, &clydev1.GetConversationInfoRequest{
		ConversationId: conversationID,
	})
	if err != nil {
		return conversation.Info{}, daemonRPCError(rpcCtx, "get conversation info", err)
	}
	return conversationInfoFromProto(resp), nil
}

// GetConversationContext asks the daemon for the messages around a center point
// rendered as plain text. loadRules is the loading-rules tag a search hit
// carried for the row that produced messageIndex; empty means the default
// rules.
func GetConversationContext(ctx context.Context, conversationID, timestamp string, messageIndex, before, after int, loadRules string) (string, error) {
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
		LoadRules:      loadRules,
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
		Offset:               int64(options.Offset),
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
			LoadRules:     wire.GetLoadRules(),
		})
	}
	return conversation.SearchConversationsResult{
		Matches:              matches,
		ConversationsScanned: int(resp.GetConversationsScanned()),
		ReturnedCount:        int(resp.GetReturnedCount()),
		Limit:                int(resp.GetLimit()),
		Offset:               int(resp.GetOffset()),
		NextOffset:           int(resp.GetNextOffset()),
		HasMore:              resp.GetHasMore(),
		Source:               searchSourceFromProto(resp.GetSource()),
		Facets:               searchFacetsFromProto(resp.GetFacets()),
		Freshness:            searchFreshnessFromProto(resp.GetFreshness()),
		FilterAccounting:     filterAccountingFromProto(resp.GetFilterAccounting()),
	}, nil
}

// ReorientConversation asks the daemon for one cursor-paged page of the
// recovered pre-compaction transcript as typed data.
func ReorientConversation(ctx context.Context, conversationID, workspace, cursor string, maxLines, pageBytes int, includeToolOutputs bool) (conversation.ReorientPage, error) {
	return reorientConversation(ctx, conversationID, workspace, cursor, maxLines, pageBytes, includeToolOutputs, false)
}

// ReorientConversationForHook asks the daemon for one reorient page and lets
// hook callers recover the current conversation before the provider compacts.
// The recovered transcript is still capped by maxLines (or the daemon default).
func ReorientConversationForHook(ctx context.Context, conversationID, workspace, cursor string, maxLines, pageBytes int, includeToolOutputs, syntheticPreCompact bool) (conversation.ReorientPage, error) {
	return reorientConversation(ctx, conversationID, workspace, cursor, maxLines, pageBytes, includeToolOutputs, syntheticPreCompact)
}

func reorientConversation(ctx context.Context, conversationID, workspace, cursor string, maxLines, pageBytes int, includeToolOutputs, syntheticPreCompact bool) (conversation.ReorientPage, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return conversation.ReorientPage{}, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, analysisClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ReorientConversation(rpcCtx, &clydev1.ReorientConversationRequest{
		ConversationId:      conversationID,
		Workspace:           workspace,
		Cursor:              cursor,
		MaxLines:            int64(maxLines),
		PageBytes:           int64(pageBytes),
		IncludeToolOutputs:  includeToolOutputs,
		SyntheticPrecompact: syntheticPreCompact,
	})
	if err != nil {
		return conversation.ReorientPage{}, daemonRPCError(rpcCtx, "reorient conversation", err)
	}
	if resp.GetCurrentConversation() == nil {
		return conversation.ReorientPage{}, fmt.Errorf("reorient conversation: daemon returned no current conversation; deploy matching daemon binary")
	}
	return reorientPageFromProto(resp), nil
}

// searchSourceFromProto maps the wire search source onto its domain enum.
func searchSourceFromProto(source clydev1.SearchSource) conversation.SearchSource {
	switch source {
	case clydev1.SearchSource_SEARCH_SOURCE_SEMANTIC:
		return conversation.SearchSourceSemantic
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

// conversationRecordFromProto rebuilds a record from the control protocol. The
// protocol carries no origin, because the daemon's index already applied the
// subagent setting before it served the record, so the client never re-decides.
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
		ID:              wire.GetId(),
		Provider:        providerFromProto(wire.GetProvider()),
		NativeID:        wire.GetNativeId(),
		Selector:        wire.GetSelector(),
		Lineage:         lineage,
		Origin:          conversation.OriginUnspecified,
		Title:           wire.GetTitle(),
		TitleUncertain:  false,
		WorkspaceRoot:   wire.GetWorkspaceRoot(),
		ArtifactPath:    wire.GetArtifactPath(),
		ArtifactKind:    wire.GetArtifactKind(),
		Model:           wire.GetModel(),
		CreatedAt:       time.Unix(wire.GetCreatedAtUnix(), 0),
		UpdatedAt:       time.Unix(wire.GetUpdatedAtUnix(), 0),
		SizeBytes:       wire.GetSizeBytes(),
		Archived:        wire.GetArchived(),
		LatestRequestID: wire.GetLatestRequestId(),
	}
}

func conversationInfoFromProto(resp *clydev1.GetConversationInfoResponse) conversation.Info {
	return conversation.Info{
		Record:          conversationRecordFromProto(resp.GetConversation()),
		Stats:           conversationStatsFromProto(resp.GetStats()),
		CompactionCount: int(resp.GetCompactionCount()),
		Segments:        conversationSegmentsFromProto(resp.GetSegments()),
	}
}

func showCaptureOutputFromProto(resp *clydev1.ShowCaptureResponse) mitmshow.ShowOutput {
	return mitmshow.ShowOutput{
		Query:       resp.GetQuery(),
		Kind:        mitmshow.IDKind(resp.GetKind()),
		Correlation: showCaptureCorrelationFromProto(resp.GetCorrelation()),
		Passes:      showCapturePassesFromProto(resp.GetPasses()),
	}
}

func legacyShowCaptureOutput(resp *clydev1.ShowCaptureResponse) string {
	if resp == nil {
		return ""
	}
	message := resp.ProtoReflect()
	field := message.Descriptor().Fields().ByName("output")
	if field == nil {
		return ""
	}
	return message.Get(field).String()
}

func showCaptureCorrelationFromProto(wire *clydev1.ShowCaptureCorrelation) mitmshow.Correlation {
	if wire == nil {
		return mitmshow.Correlation{
			ClydeRequestID:    "",
			CursorRequestID:   "",
			UpstreamRequestID: "",
			TraceID:           "",
		}
	}
	return mitmshow.Correlation{
		ClydeRequestID:    wire.GetClydeRequestId(),
		CursorRequestID:   wire.GetCursorRequestId(),
		UpstreamRequestID: wire.GetUpstreamRequestId(),
		TraceID:           wire.GetTraceId(),
	}
}

func showCapturePassesFromProto(wirePasses []*clydev1.ShowCapturePass) []mitmshow.LookupPass {
	passes := make([]mitmshow.LookupPass, 0, len(wirePasses))
	for _, wire := range wirePasses {
		passes = append(passes, mitmshow.LookupPass{
			ID:       wire.GetId(),
			Sections: showCaptureSectionsFromProto(wire.GetSections()),
			Capture:  showCaptureRowsFromProto(wire.GetCapture()),
			Found:    showCaptureCorrelationFromProto(wire.GetFound()),
		})
	}
	return passes
}

func showCaptureSectionsFromProto(wireSections []*clydev1.ShowCaptureSection) []mitmshow.Section {
	sections := make([]mitmshow.Section, 0, len(wireSections))
	for _, wire := range wireSections {
		sections = append(sections, mitmshow.Section{
			Source:  wire.GetSource(),
			Path:    wire.GetPath(),
			Matches: wire.GetMatches(),
		})
	}
	return sections
}

func showCaptureRowsFromProto(wire *clydev1.ShowCaptureRows) mitmshow.CaptureSection {
	if wire == nil {
		return mitmshow.CaptureSection{
			Source: "",
			Path:   "",
			Rows:   nil,
		}
	}
	rows := make([]mitmshow.CaptureRow, 0, len(wire.GetRows()))
	for _, item := range wire.GetRows() {
		rows = append(rows, mitmshow.CaptureRow{
			Timestamp:         item.GetTs(),
			Client:            item.GetClient(),
			Provider:          item.GetProvider(),
			Concern:           item.GetConcern(),
			Host:              item.GetHost(),
			Method:            item.GetMethod(),
			Path:              item.GetPath(),
			Status:            int(item.GetStatus()),
			RequestID:         item.GetRequestId(),
			UpstreamRequestID: item.GetUpstreamRequestId(),
			SessionID:         item.GetSessionId(),
			TraceID:           item.GetTraceId(),
		})
	}
	return mitmshow.CaptureSection{
		Source: wire.GetSource(),
		Path:   wire.GetPath(),
		Rows:   rows,
	}
}

func logsInventoryFromProto(resp *clydev1.LogsInventoryResponse) loginventory.Inventory {
	generated := time.Time{}
	if resp.GetGeneratedUnix() > 0 {
		generated = time.Unix(resp.GetGeneratedUnix(), 0)
	}
	return loginventory.Inventory{
		StateRoot:      resp.GetStateRoot(),
		Generated:      generated,
		Mode:           loginventory.InventoryMode(resp.GetMode()),
		CleanupEnabled: resp.GetCleanupEnabled(),
		Categories:     logsInventoryCategoriesFromProto(resp.GetCategories()),
	}
}

func logsInventoryCategoriesFromProto(wireCategories []*clydev1.LogsInventoryCategory) []loginventory.CategorySummary {
	categories := make([]loginventory.CategorySummary, 0, len(wireCategories))
	for _, wire := range wireCategories {
		categories = append(categories, loginventory.CategorySummary{
			Category:           loginventory.Category(wire.GetCategory()),
			Sink:               wire.GetSink(),
			Source:             loginventory.InventorySource(wire.GetSource()),
			Count:              int(wire.GetCount()),
			TotalBytes:         wire.GetTotalBytes(),
			LatestModified:     unixOrZero(wire.GetLatestModifiedUnix()),
			RepresentativePath: wire.GetRepresentativePath(),
			LastEventTimestamp: unixOrZero(wire.GetLastEventTimestampUnix()),
			LastEventRequestID: wire.GetLastEventRequestId(),
			LastCleanupResult:  logsInventoryCleanupSummaryFromProto(wire.GetLastCleanupResult()),
			CleanupEnabled:     wire.GetCleanupEnabled(),
			Rotation:           logsInventoryRotationFromProto(wire.GetRotation()),
			Cleanup:            logsInventoryCleanupFromProto(wire.GetCleanup()),
			LargestFiles:       logsInventoryFilesFromProto(wire.GetLargestFiles()),
		})
	}
	return categories
}

func logsInventoryRotationFromProto(wire *clydev1.LogsInventoryRotation) config.LoggingRotation {
	if wire == nil {
		return config.LoggingRotation{
			Enabled:    nil,
			MaxSizeMB:  0,
			MaxBackups: 0,
			MaxAgeDays: 0,
			Compress:   nil,
		}
	}
	return config.LoggingRotation{
		Enabled:    wire.Enabled,
		MaxSizeMB:  int(wire.GetMaxSizeMb()),
		MaxBackups: int(wire.GetMaxBackups()),
		MaxAgeDays: int(wire.GetMaxAgeDays()),
		Compress:   wire.Compress,
	}
}

func logsInventoryCleanupFromProto(wire *clydev1.LogsInventoryCleanup) config.LoggingCleanup {
	if wire == nil {
		return config.LoggingCleanup{
			Enabled:    nil,
			MaxAgeDays: nil,
			MaxBackups: nil,
			MaxTotalMB: nil,
		}
	}
	return config.LoggingCleanup{
		Enabled:    wire.Enabled,
		MaxAgeDays: int64PtrToIntPtr(wire.MaxAgeDays),
		MaxBackups: int64PtrToIntPtr(wire.MaxBackups),
		MaxTotalMB: int64PtrToIntPtr(wire.MaxTotalMb),
	}
}

func logsInventoryFilesFromProto(wireFiles []*clydev1.LogsInventoryFileSummary) []loginventory.FileSummary {
	files := make([]loginventory.FileSummary, 0, len(wireFiles))
	for _, wire := range wireFiles {
		files = append(files, loginventory.FileSummary{
			RelativePath: wire.GetRelativePath(),
			SizeBytes:    wire.GetSizeBytes(),
			Modified:     unixOrZero(wire.GetModifiedUnix()),
		})
	}
	return files
}

func logsInventoryCleanupSummaryFromProto(wire *clydev1.LogsInventoryCleanupSummary) *loginventory.InventoryCleanupSummary {
	if wire == nil {
		return nil
	}
	return &loginventory.InventoryCleanupSummary{
		Timestamp:    unixOrZero(wire.GetTimestampUnix()),
		Root:         wire.GetRoot(),
		ScannedRoots: wire.GetScannedRoots(),
		Candidates:   int(wire.GetCandidates()),
		Deleted:      int(wire.GetDeleted()),
		BytesDeleted: wire.GetBytesDeleted(),
		Skipped:      wire.GetSkipped(),
		Errors:       wire.GetErrors(),
		DurationMS:   wire.GetDurationMs(),
	}
}

func unixOrZero(unix int64) time.Time {
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

func int64PtrToIntPtr(value *int64) *int {
	if value == nil {
		return nil
	}
	intValue := int(*value)
	return &intValue
}

func conversationStatsFromProto(wire *clydev1.ConversationInfoStats) conversation.Stats {
	if wire == nil {
		return conversation.Stats{
			TotalMessages:     0,
			VisibleMessages:   0,
			UserMessages:      0,
			AssistantMessages: 0,
			SystemMessages:    0,
			ToolCallCount:     0,
			ToolOutputCount:   0,
		}
	}
	return conversation.Stats{
		TotalMessages:     int(wire.GetTotalMessages()),
		VisibleMessages:   int(wire.GetVisibleMessages()),
		UserMessages:      int(wire.GetUserMessages()),
		AssistantMessages: int(wire.GetAssistantMessages()),
		SystemMessages:    int(wire.GetSystemMessages()),
		ToolCallCount:     int(wire.GetToolCallCount()),
		ToolOutputCount:   int(wire.GetToolOutputCount()),
	}
}

func conversationSegmentsFromProto(
	wireSegments []*clydev1.ConversationCompactionSegment,
) []conversation.CompactionSegment {
	segments := make([]conversation.CompactionSegment, 0, len(wireSegments))
	for _, wire := range wireSegments {
		summaryTimestamp := time.Time{}
		if wire.GetHasStartingSummary() {
			summaryTimestamp = time.Unix(wire.GetSummaryTimestampUnix(), 0)
		}
		segments = append(segments, conversation.CompactionSegment{
			Index:               int(wire.GetIndex()),
			StartMessageIndex:   int(wire.GetStartMessageIndex()),
			EndMessageIndex:     int(wire.GetEndMessageIndex()),
			HasStartingSummary:  wire.GetHasStartingSummary(),
			SummaryMessageIndex: int(wire.GetSummaryMessageIndex()),
			SummaryUUID:         wire.GetSummaryUuid(),
			SummaryTimestamp:    summaryTimestamp,
			VisibleMessageCount: int(wire.GetVisibleMessageCount()),
			ToolCallCount:       int(wire.GetToolCallCount()),
			Checkpoint: conversation.CompactionCheckpoint{
				BoundaryIndex:           -1,
				BoundaryUUID:            "",
				SummaryIndex:            -1,
				SummaryUUID:             "",
				ContextItems:            nil,
				Trigger:                 "",
				HeadUUID:                "",
				AnchorUUID:              "",
				TailUUID:                "",
				MessagesSummarized:      0,
				ReplacementHistoryCount: 0,
			},
		})
	}
	return segments
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
		LastN:                  int64(options.LastN),
		MaxLines:               int64(options.MaxLines),
		MaxTokens:              options.MaxTokens,
		TokenModel:             options.TokenModel,
		IncludeSystemPrompts:   options.Content.Has(conversation.ContentKindSystemPrompts),
		IncludeSystemMessages:  options.Content.Has(conversation.ContentKindSystemMessages),
		IncludeToolOutputs:     options.Content.Has(conversation.ContentKindToolOutputs),
		IncludeRawJsonMetadata: options.Content.Has(conversation.ContentKindRawJSONMetadata),
		IncludeThinking:        options.Content.Has(conversation.ContentKindThinking),
		IncludeToolSummaries:   options.Content.Has(conversation.ContentKindToolSummaries),
		IncludeToolCalls:       options.Content.Has(conversation.ContentKindToolCalls),
		IncludeChat:            options.Content.Has(conversation.ContentKindChat),
		IncludeInjected:        options.Content.Has(conversation.ContentKindInjected),
		IncludeCompactions:     options.Compaction.IncludeSelector,
		FullHistory:            options.Compaction.FullHistory,
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
// the SQLite capture store and returns the typed lookup result.
func ShowCapture(ctx context.Context, id string) (mitmshow.ShowOutput, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return mitmshow.ShowOutput{}, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.ShowCapture(rpcCtx, &clydev1.ShowCaptureRequest{Id: id})
	if err != nil {
		return mitmshow.ShowOutput{}, daemonRPCError(rpcCtx, "show capture", err)
	}
	if len(resp.GetPasses()) == 0 && legacyShowCaptureOutput(resp) != "" {
		return mitmshow.ShowOutput{}, fmt.Errorf("show capture: daemon returned legacy text-only response; deploy matching daemon binary")
	}
	return showCaptureOutputFromProto(resp), nil
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
// files and return it as typed data.
func LogsInventory(ctx context.Context, stateRoot string, largestFileLimit int, deep bool) (loginventory.Inventory, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return loginventory.Inventory{}, err
	}
	defer func() { _ = client.conn.Close() }()

	rpcCtx, cancel := context.WithTimeout(ctx, queryClientRPCTimeout)
	defer cancel()
	resp, err := client.rpc.LogsInventory(rpcCtx, &clydev1.LogsInventoryRequest{
		StateRoot:        stateRoot,
		LargestFileLimit: int64(largestFileLimit),
		Deep:             deep,
	})
	if err != nil {
		return loginventory.Inventory{}, daemonRPCError(rpcCtx, "logs inventory", err)
	}
	return logsInventoryFromProto(resp), nil
}

// daemonRPCError translates a failed control-plane rpc into a caller-facing
// error. An Unavailable code means the daemon is not running, so the message
// names the rpc target and how to check the daemon.
func daemonRPCError(ctx context.Context, operation string, err error) error {
	target := daemonGRPCAddress()
	if status.Code(err) == codes.Unavailable {
		slog.WarnContext(ctx, "daemon.client.rpc.unavailable", "concern", "process.daemon.lifecycle", "component", "daemon",
			"operation", operation,
			"grpc_address", target,
			"err", err,
		)
		return fmt.Errorf("clyde daemon is not running at %s; check `clyde daemon status` (launchd starts the daemon): %w", target, err)
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
	target := daemonGRPCAddress()
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(controlMaxMessageBytes),
			grpc.MaxCallSendMsgSize(controlMaxMessageBytes),
		),
	)
	if err != nil {
		slog.WarnContext(ctx, "daemon.client.connect.new_client_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"grpc_address", target,
			"err", err,
		)
		return nil, fmt.Errorf("connect daemon at %s: %w", target, err)
	}
	return &daemonClient{
		conn: conn,
		rpc:  clydev1.NewClydeServiceClient(conn),
	}, nil
}

func daemonGRPCAddress() string {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		// Surface the config error before falling back so an operator sees why
		// the CLI targeted the default socket instead of their configured one.
		slog.Warn("daemon.client.grpc_address.load_failed",
			"concern", "process.daemon.lifecycle", "component", "daemon",
			"fallback", config.DefaultDaemonGRPCAddress(), "err", err)
		return config.DefaultDaemonGRPCAddress()
	}
	return cfg.Daemon.GRPCAddress
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
