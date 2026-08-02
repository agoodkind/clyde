package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/loginventory"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/mitmshow"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/gklog/correlation"
)

const providerStatsStreamInterval = time.Second

type controlServer struct {
	clydev1.UnimplementedClydeServiceServer
	stats         *providerStatsRecorder
	index         *conversation.Index
	loggingConfig config.LoggingConfig
	mitmConfig    config.MITMConfig
	mitmStatus    func() MITMStatus
	showCapture   func(ctx context.Context, id string) (mitmshow.ShowOutput, error)
	reload        func(context.Context) (*clydev1.ReloadDaemonResponse, error)
	rebind        func(context.Context) (*clydev1.ReloadDaemonResponse, error)
	// searchSource is the only cross-conversation lookup boundary.
	searchSource conversationSearchSource
	// freshness reports the conversation-index sync snapshot at query time, set
	// at construction. Nil yields the zero-value freshness.
	freshness func() conversation.SearchFreshness
	// captureStore is the daemon's shared SQLite capture store. SeedBaseline
	// reads the deduped shape corpus from it and writes the baseline through
	// it; nil when MITM is disabled.
	captureStore *capture.Store
	// exportTokens configures the --max-tokens export cap: the local estimator
	// tuning, credentials, and endpoints for the exact provider count APIs.
	exportTokens       exportTokenConfig
	providerStatsNow   func() time.Time
	providerStatsTicks <-chan time.Time
}

// conversationSemanticSearchClient is the vector engine client adapted by
// semanticConversationSearchSource. The semsearch client satisfies it and tests
// supply a fake.
type conversationSemanticSearchClient interface {
	SearchConversations(ctx context.Context, collectionID, query string, limit int32, filter semsearch.SearchFilter, perConversationLimit int32) ([]semsearch.SemHit, error)
	SearchWithinConversation(ctx context.Context, collectionID, conversationID, query string, limit int32, filter semsearch.SearchFilter) ([]semsearch.SemHit, string, error)
}

func (s *controlServer) ReloadDaemon(ctx context.Context, _ *clydev1.ReloadDaemonRequest) (*clydev1.ReloadDaemonResponse, error) {
	if s.reload == nil {
		return nil, status.Error(codes.FailedPrecondition, "daemon reload is not available")
	}
	return s.reload(ctx)
}

func (s *controlServer) RebindDaemon(ctx context.Context, _ *clydev1.ReloadDaemonRequest) (*clydev1.ReloadDaemonResponse, error) {
	if s.rebind == nil {
		return nil, status.Error(codes.FailedPrecondition, "daemon rebind is not available")
	}
	return s.rebind(ctx)
}

func (s *controlServer) GetProviderStats(context.Context, *clydev1.GetProviderStatsRequest) (*clydev1.GetProviderStatsResponse, error) {
	return providerStatsResponse(s.stats), nil
}

// ListConversations answers the conversation index that the daemon refreshes in
// the background. It is the one place that reads the index for every front end.
func (s *controlServer) ListConversations(ctx context.Context, req *clydev1.ListConversationsRequest) (*clydev1.ListConversationsResponse, error) {
	result, err := s.index.ListPage(ctx, conversation.ListOptions{
		Limit:           int(req.GetLimit()),
		Offset:          int(req.GetOffset()),
		Provider:        providerFromProto(req.GetProvider()),
		WorkspaceRoot:   req.GetWorkspace(),
		Query:           req.GetQuery(),
		IncludeArchived: req.GetIncludeArchived(),
		All:             req.GetAll(),
	})
	if err != nil {
		client, _ := peer.FromContext(ctx)
		slog.WarnContext(ctx, "daemon.list_conversations.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "list conversations: %v", err)
	}
	out := make([]*clydev1.ConversationRecord, 0, len(result.Records))
	for _, record := range result.Records {
		out = append(out, protoConversationRecord(ctx, s.index, record))
	}
	return &clydev1.ListConversationsResponse{
		Conversations: out,
		TotalMatched:  int64(result.TotalMatched),
		ReturnedCount: int64(result.ReturnedCount),
		Offset:        int64(result.Offset),
		Limit:         int64(result.Limit),
		NextOffset:    int64(result.NextOffset),
		HasMore:       result.HasMore,
	}, nil
}

// GetConversationInfo resolves one conversation and returns static metadata
// plus its compaction segment stack.
func (s *controlServer) GetConversationInfo(ctx context.Context, req *clydev1.GetConversationInfoRequest) (*clydev1.GetConversationInfoResponse, error) {
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.get_conversation_info.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return nil, status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	info, err := s.index.ConversationInfo(record)
	if err != nil {
		slog.WarnContext(ctx, "daemon.get_conversation_info.load_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "get conversation info: %v", err)
	}
	return protoConversationInfo(ctx, s.index, info), nil
}

// SearchConversations returns a relevance-ranked list of conversation hits.
// Each hit carries the matched passage as a byte-bounded excerpt (set in
// engineSearchMatches), so the list is self-sufficient for triage and small
// enough for any transport. The full surrounding window is a separate windowed
// read; search never inlines it. The freshness snapshot lets a thin result be
// distinguished from a cold index.
func (s *controlServer) SearchConversations(ctx context.Context, req *clydev1.SearchConversationsRequest) (*clydev1.SearchConversationsResponse, error) {
	// Establish correlation before any blocking search work so the operation is
	// traceable, including a source failure.
	ctx, _ = correlation.Ensure(ctx, "")
	client, _ := peer.FromContext(ctx)
	if req.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	if s.searchSource == nil {
		failure := unavailableConversationSearchSourceError(nil)
		return nil, status.Error(failure.grpcCode(), failure.Error())
	}
	result, err := s.searchSource.SearchConversations(ctx, searchConversationsOptionsFromProto(req))
	if err != nil {
		var invalidBounds invalidSearchBoundsError
		if errors.As(err, &invalidBounds) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		var sourceFailure conversationSearchSourceError
		if errors.As(err, &sourceFailure) {
			slog.WarnContext(ctx, "daemon.search_conversations.source_failed",
				"concern", "process.daemon.lifecycle",
				"component", "daemon",
				"peer", peerString(client),
				"failure_code", string(sourceFailure.code),
				"err", sourceFailure.cause,
			)
			return nil, status.Error(sourceFailure.grpcCode(), sourceFailure.Error())
		}
		slog.WarnContext(ctx, "daemon.search_conversations.failed",
			"concern", "process.daemon.lifecycle",
			"component", "daemon",
			"peer", peerString(client),
			"err", err,
		)
		failure := failedConversationSearchSourceError(err)
		return nil, status.Error(failure.grpcCode(), failure.Error())
	}
	result.Freshness = s.freshnessSnapshot()
	return searchConversationsResponse(ctx, s.index, result), nil
}

func searchConversationsOptionsFromProto(req *clydev1.SearchConversationsRequest) conversation.SearchConversationsOptions {
	return conversation.SearchConversationsOptions{
		Query:                req.GetQuery(),
		Limit:                int(req.GetLimit()),
		Offset:               int(req.GetOffset()),
		Provider:             providerFromProto(req.GetProvider()),
		WorkspaceRoot:        req.GetWorkspace(),
		IncludeArchived:      req.GetIncludeArchived(),
		Roles:                req.GetRoles(),
		FromUnix:             req.GetFromUnix(),
		UntilUnix:            req.GetUntilUnix(),
		MinScore:             req.GetMinScore(),
		PerConversationLimit: int(req.GetPerConversationLimit()),
		ConversationID:       req.GetConversationId(),
		ContextWindow:        int(req.GetContextWindow()),
	}
}

// ReorientConversation builds the recovered pre-compaction transcript for one
// conversation and returns one cursor-paged page of it. The page is bounded so
// it renders inline, and the caller loops on next_cursor until remaining is zero.
func (s *controlServer) ReorientConversation(ctx context.Context, req *clydev1.ReorientConversationRequest) (*clydev1.ReorientConversationResponse, error) {
	ctx, _ = correlation.Ensure(ctx, "")
	page, err := s.index.ReorientPage(ctx, conversation.ReorientOptions{
		ConversationID:      req.GetConversationId(),
		WorkspaceRoot:       req.GetWorkspace(),
		MaxLines:            int(req.GetMaxLines()),
		MaxBytes:            0,
		IncludeToolOutputs:  req.GetIncludeToolOutputs(),
		SyntheticPreCompact: req.GetSyntheticPrecompact(),
	}, req.GetCursor(), int(req.GetPageBytes()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reorient conversation: %v", err)
	}
	return protoReorientPage(page), nil
}

// freshnessSnapshot reads the conversation-index sync snapshot, returning the
// zero value when no freshness source is wired.
func (s *controlServer) freshnessSnapshot() conversation.SearchFreshness {
	if s.freshness == nil {
		return conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0}
	}
	return s.freshness()
}

// searchFacetTopN bounds each facet dimension to its top values by count.
const searchFacetTopN = 5

// filterAccounting builds the ordered candidate-count funnel from the index:
// the indexed baseline first, then one stage per active filter, computed by
// reusing ConversationIDsMatching. A stage whose count cannot be resolved is
// omitted rather than fabricated; the indexed baseline and the caller-appended
// returned stage keep the funnel honest.
func filterAccounting(ctx context.Context, idx conversationSearchIndex, options conversation.SearchConversationsOptions) []conversation.FilterStage {
	var anyProvider conversation.Provider
	stages := make([]conversation.FilterStage, 0, 5)
	if all, err := idx.ConversationIDsMatching(ctx, anyProvider, "", true); err == nil {
		stages = append(stages, conversation.FilterStage{Name: "indexed", Remaining: len(all)})
	}
	provider := options.Provider
	if provider.Valid() {
		if matched, err := idx.ConversationIDsMatching(ctx, provider, "", true); err == nil {
			stages = append(stages, conversation.FilterStage{Name: "provider", Remaining: len(matched)})
		}
	}
	if options.WorkspaceRoot != "" {
		if matched, err := idx.ConversationIDsMatching(ctx, provider, options.WorkspaceRoot, true); err == nil {
			stages = append(stages, conversation.FilterStage{Name: "workspace", Remaining: len(matched)})
		}
	}
	if !options.IncludeArchived {
		if matched, err := idx.ConversationIDsMatching(ctx, provider, options.WorkspaceRoot, false); err == nil {
			stages = append(stages, conversation.FilterStage{Name: "archived_excluded", Remaining: len(matched)})
		}
	}
	if options.ConversationID != "" {
		stages = append(stages, conversation.FilterStage{Name: "conversation", Remaining: 1})
	}
	return stages
}

// appendReturnedStage closes the funnel with the count actually returned.
func appendReturnedStage(stages []conversation.FilterStage, returned int) []conversation.FilterStage {
	return append(stages, conversation.FilterStage{Name: "returned", Remaining: returned})
}

// searchConversationsResponse maps the cross-conversation search result onto its
// wire response, carrying the source, facets, freshness, filter funnel, and the
// per-hit inline context window.
func searchConversationsResponse(ctx context.Context, idx *conversation.Index, result conversation.SearchConversationsResult) *clydev1.SearchConversationsResponse {
	matches := make([]*clydev1.ConversationSearchMatch, 0, len(result.Matches))
	for _, match := range result.Matches {
		matches = append(matches, &clydev1.ConversationSearchMatch{
			Conversation:  protoConversationRecord(ctx, idx, match.Record),
			MessageIndex:  int64(match.MessageIndex),
			Role:          match.Role,
			TimestampUnix: match.Timestamp.Unix(),
			Snippet:       match.Snippet,
			Score:         match.Score,
			ContextWindow: match.ContextWindow,
		})
	}
	return &clydev1.SearchConversationsResponse{
		Matches:              matches,
		ConversationsScanned: int64(result.ConversationsScanned),
		ReturnedCount:        int64(result.ReturnedCount),
		Limit:                int64(result.Limit),
		Offset:               int64(result.Offset),
		NextOffset:           int64(result.NextOffset),
		HasMore:              result.HasMore,
		Source:               protoSearchSource(result.Source),
		Facets:               protoSearchFacets(result.Facets),
		Freshness:            protoSearchFreshness(result.Freshness),
		FilterAccounting:     protoFilterAccounting(result.FilterAccounting),
	}
}

// protoSearchSource maps the domain search source onto its wire enum, mirroring
// the protoProvider switch pattern.
func protoSearchSource(source conversation.SearchSource) clydev1.SearchSource {
	switch source {
	case conversation.SearchSourceSemantic:
		return clydev1.SearchSource_SEARCH_SOURCE_SEMANTIC
	case conversation.SearchSourceUnspecified:
		return clydev1.SearchSource_SEARCH_SOURCE_UNSPECIFIED
	default:
		return clydev1.SearchSource_SEARCH_SOURCE_UNSPECIFIED
	}
}

// protoSearchFacets maps the domain facets onto the wire facets.
func protoSearchFacets(facets conversation.SearchFacets) *clydev1.SearchFacets {
	return &clydev1.SearchFacets{
		Workspaces: protoFacetCounts(facets.Workspaces),
		Providers:  protoFacetCounts(facets.Providers),
		Models:     protoFacetCounts(facets.Models),
	}
}

// protoFacetCounts maps one facet dimension onto its wire counts.
func protoFacetCounts(counts []conversation.SearchFacetCount) []*clydev1.SearchFacetCount {
	out := make([]*clydev1.SearchFacetCount, 0, len(counts))
	for _, count := range counts {
		out = append(out, &clydev1.SearchFacetCount{Value: count.Value, Count: int64(count.Count)})
	}
	return out
}

// protoSearchFreshness maps the domain freshness onto its wire form.
func protoSearchFreshness(freshness conversation.SearchFreshness) *clydev1.SearchFreshness {
	return &clydev1.SearchFreshness{
		Manifest:     int64(freshness.Manifest),
		Needed:       int64(freshness.Needed),
		Embedded:     int64(freshness.Embedded),
		Pending:      int64(freshness.Pending),
		LastSyncUnix: freshness.LastSyncUnix,
	}
}

// protoFilterAccounting maps the domain filter funnel onto its wire form.
func protoFilterAccounting(stages []conversation.FilterStage) *clydev1.FilterAccounting {
	out := make([]*clydev1.FilterStage, 0, len(stages))
	for _, stage := range stages {
		out = append(out, &clydev1.FilterStage{Name: stage.Name, Remaining: int64(stage.Remaining)})
	}
	return &clydev1.FilterAccounting{Stages: out}
}

// contentKindSetFromExportRequest rebuilds the content-kind set from the export
// request's per-kind booleans, which remain the wire encoding of the selection.
func contentKindSetFromExportRequest(req *clydev1.ExportTranscriptRequest) conversation.ContentKindSet {
	var kinds []conversation.ContentKind
	if req.GetIncludeChat() {
		kinds = append(kinds, conversation.ContentKindChat)
	}
	if req.GetIncludeThinking() {
		kinds = append(kinds, conversation.ContentKindThinking)
	}
	if req.GetIncludeToolSummaries() {
		kinds = append(kinds, conversation.ContentKindToolSummaries)
	}
	if req.GetIncludeToolCalls() {
		kinds = append(kinds, conversation.ContentKindToolCalls)
	}
	if req.GetIncludeToolOutputs() {
		kinds = append(kinds, conversation.ContentKindToolOutputs)
	}
	if req.GetIncludeSystemPrompts() {
		kinds = append(kinds, conversation.ContentKindSystemPrompts)
	}
	if req.GetIncludeSystemMessages() {
		kinds = append(kinds, conversation.ContentKindSystemMessages)
	}
	if req.GetIncludeInjected() {
		kinds = append(kinds, conversation.ContentKindInjected)
	}
	if req.GetIncludeRawJsonMetadata() {
		kinds = append(kinds, conversation.ContentKindRawJSONMetadata)
	}
	return conversation.NewContentKindSet(kinds...)
}

// GetMITMStatus reports each configured MITM listener address, whether the
// daemon has it bound, and the CA paths.
func (s *controlServer) GetMITMStatus(context.Context, *clydev1.GetMITMStatusRequest) (*clydev1.GetMITMStatusResponse, error) {
	snapshot := MITMStatus{Listeners: nil, CACertPath: "", CAKeyPath: ""}
	if s.mitmStatus != nil {
		snapshot = s.mitmStatus()
	}
	listeners := make([]*clydev1.MITMListenerStatus, 0, len(snapshot.Listeners))
	for _, listener := range snapshot.Listeners {
		listeners = append(listeners, &clydev1.MITMListenerStatus{
			Id:      listener.ID,
			Address: listener.Address,
			Up:      listener.Up,
		})
	}
	return &clydev1.GetMITMStatusResponse{
		Listeners:  listeners,
		CaCertPath: snapshot.CACertPath,
		CaKeyPath:  snapshot.CAKeyPath,
	}, nil
}

// ShowCapture correlates one request id across the daemon's logs and the SQLite
// capture store and returns the typed lookup result.
func (s *controlServer) ShowCapture(ctx context.Context, req *clydev1.ShowCaptureRequest) (*clydev1.ShowCaptureResponse, error) {
	if s.showCapture == nil {
		return &clydev1.ShowCaptureResponse{
			Query:       req.GetId(),
			Kind:        string(mitmshow.ClassifyID(req.GetId())),
			Correlation: &clydev1.ShowCaptureCorrelation{},
			Passes:      nil,
		}, nil
	}
	output, err := s.showCapture(ctx, req.GetId())
	if err != nil {
		client, _ := peer.FromContext(ctx)
		slog.WarnContext(ctx, "daemon.show_capture.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "show capture: %v", err)
	}
	return protoShowCaptureOutput(output), nil
}

// SeedBaseline builds a wire baseline from the capture store's deduped shape
// corpus and writes it as the current baseline for the given upstream.
func (s *controlServer) SeedBaseline(ctx context.Context, req *clydev1.SeedBaselineRequest) (*clydev1.SeedBaselineResponse, error) {
	result, err := mitm.SeedBaseline(ctx, s.captureStore, req.GetUpstream(), req.GetIncludeUa(), req.GetExcludeUa())
	if err != nil {
		client, _ := peer.FromContext(ctx)
		slog.WarnContext(ctx, "daemon.seed_baseline.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"upstream", req.GetUpstream(),
			"err", err,
		)
		return nil, status.Errorf(codes.InvalidArgument, "seed baseline: %v", err)
	}
	return &clydev1.SeedBaselineResponse{
		Written:  "",
		Upstream: result.Upstream,
		Flavors:  int64(result.Flavors),
	}, nil
}

// LogsInventory builds a metadata-only inventory of the daemon's log files and
// returns it as typed data for the client-side renderer.
func (s *controlServer) LogsInventory(ctx context.Context, req *clydev1.LogsInventoryRequest) (*clydev1.LogsInventoryResponse, error) {
	output, err := loginventory.Build(
		req.GetStateRoot(),
		int(req.GetLargestFileLimit()),
		req.GetDeep(),
		s.loggingConfig,
		s.mitmConfig,
	)
	if err != nil {
		client, _ := peer.FromContext(ctx)
		slog.WarnContext(ctx, "daemon.logs_inventory.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "logs inventory: %v", err)
	}
	return protoLogsInventory(output), nil
}

func (s *controlServer) SubscribeProviderStats(_ *clydev1.SubscribeProviderStatsRequest, stream grpc.ServerStreamingServer[clydev1.ProviderStatsEvent]) error {
	now := s.providerStatsNow
	if now == nil {
		now = clock.Now
	}
	ticks := s.providerStatsTicks
	stop := func() {}
	if ticks == nil {
		ticker := time.NewTicker(providerStatsStreamInterval)
		ticks = ticker.C
		stop = ticker.Stop
	}
	defer stop()
	for {
		response := providerStatsResponse(s.stats)
		emittedAtUnix := now().Unix()
		for _, stats := range response.GetProviders() {
			event := &clydev1.ProviderStatsEvent{
				Stats:         stats,
				EmittedAtUnix: emittedAtUnix,
			}
			if err := stream.Send(event); err != nil {
				slog.WarnContext(stream.Context(), "daemon.provider_stats.stream_send_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
					"err", err,
				)
				return fmt.Errorf("send provider stats event: %w", err)
			}
		}
		select {
		case <-stream.Context().Done():
			slog.WarnContext(stream.Context(), "daemon.provider_stats.stream_context_done", "concern", "process.daemon.lifecycle", "component", "daemon",
				"err", stream.Context().Err(),
			)
			return fmt.Errorf("provider stats stream context: %w", stream.Context().Err())
		case <-ticks:
		}
	}
}

func providerStatsResponse(recorder *providerStatsRecorder) *clydev1.GetProviderStatsResponse {
	snapshot, loadedAt := recorder.snapshot()
	out := make([]*clydev1.ProviderStats, 0, len(snapshot))
	for _, stats := range snapshot {
		out = append(out, &clydev1.ProviderStats{
			Provider:                   protoProvider(stats.Provider),
			Requests:                   int32FromInt(stats.Requests),
			Inflight:                   int32FromInt(stats.Inflight),
			Streaming:                  int32FromInt(stats.Streaming),
			InputTokens:                stats.InputTokens,
			OutputTokens:               stats.OutputTokens,
			CacheReadTokens:            stats.CacheReadTokens,
			CacheCreationTokens:        stats.CacheCreationTokens,
			HitRatio:                   stats.HitRatio,
			EstimatedCostMicrocents:    stats.EstimatedCostMicrocents,
			LastSeenUnix:               stats.LastSeenUnix,
			Error:                      stats.Error,
			DerivedCacheCreationTokens: stats.DerivedCacheCreationTokens,
			ProviderDetail:             stats.ProviderDetail,
		})
	}
	return &clydev1.GetProviderStatsResponse{
		Providers:    out,
		LoadedAtUnix: loadedAt.Unix(),
	}
}

func int32FromInt(value int) int32 {
	const minInt32Value = -2147483648
	if value > maxInt32Value {
		return maxInt32Value
	}
	if value < minInt32Value {
		return minInt32Value
	}
	return int32(value)
}

// peerString formats a gRPC peer for log enrichment, returning an empty string
// when no addressable peer is present.
func peerString(client *peer.Peer) string {
	if client != nil && client.Addr != nil {
		return client.Addr.String()
	}
	return ""
}

// protoConversationRecord maps a conversation record onto its wire form. When
// the record carries fork lineage whose parent is present in idx, it sets
// parent_conversation_id to the resolved parent's derived id; spawn, unknown, or
// unresolvable parents leave it empty. The parent lookup is a cheap snapshot read
// with no index refresh.
func protoConversationRecord(ctx context.Context, idx *conversation.Index, record conversation.Record) *clydev1.ConversationRecord {
	wire := &clydev1.ConversationRecord{
		Id:              record.ID,
		Provider:        protoProvider(record.Provider),
		NativeId:        record.NativeID,
		Title:           record.Title,
		WorkspaceRoot:   record.WorkspaceRoot,
		ArtifactPath:    record.ArtifactPath,
		ArtifactKind:    record.ArtifactKind,
		Model:           record.Model,
		CreatedAtUnix:   record.CreatedAt.Unix(),
		UpdatedAtUnix:   record.UpdatedAt.Unix(),
		SizeBytes:       record.SizeBytes,
		Archived:        record.Archived,
		LatestRequestId: record.LatestRequestID,
	}
	if record.Lineage != nil {
		wire.Lineage = &clydev1.ConversationLineage{
			Kind:              string(record.Lineage.Kind),
			ParentProvider:    protoProvider(record.Lineage.ParentProvider),
			ParentNativeId:    record.Lineage.ParentNativeID,
			ParentMessageUuid: record.Lineage.ParentMessageUUID,
		}
	}
	if parent, ok, err := conversation.ResolveForkParent(ctx, idx, record); err == nil && ok {
		wire.ParentConversationId = parent.ID
	}
	return wire
}

func protoConversationInfo(
	ctx context.Context,
	idx *conversation.Index,
	info conversation.Info,
) *clydev1.GetConversationInfoResponse {
	return &clydev1.GetConversationInfoResponse{
		Conversation:    protoConversationRecord(ctx, idx, info.Record),
		Stats:           protoConversationStats(info.Stats),
		CompactionCount: int64(info.CompactionCount),
		Segments:        protoConversationSegments(info.Segments),
	}
}

func protoReorientPage(page conversation.ReorientPage) *clydev1.ReorientConversationResponse {
	return &clydev1.ReorientConversationResponse{
		CurrentConversation: protoReorientConversationRef(page.CurrentConversation),
		PageBody:            []byte(page.Body),
		NextCursor:          page.NextCursor,
		Remaining:           int64(page.Remaining),
		Offset:              int64(page.Offset),
		TotalBytes:          int64(page.TotalBytes),
		TotalLines:          int64(page.TotalLines),
		Truncated:           page.Truncated,
		Restart:             page.Restart,
		Warnings:            page.Warnings,
	}
}

func protoReorientConversationRef(ref conversation.ReorientConversationRef) *clydev1.ReorientConversationRef {
	provider, ok := providerid.Parse(ref.Provider)
	if !ok {
		provider = providerid.ProviderUnspecified
	}
	return &clydev1.ReorientConversationRef{
		Id:            ref.ID,
		Provider:      protoProvider(provider),
		Title:         ref.Title,
		WorkspaceRoot: ref.WorkspaceRoot,
	}
}

func protoShowCaptureOutput(output mitmshow.ShowOutput) *clydev1.ShowCaptureResponse {
	return &clydev1.ShowCaptureResponse{
		Query:       output.Query,
		Kind:        string(output.Kind),
		Correlation: protoShowCaptureCorrelation(output.Correlation),
		Passes:      protoShowCapturePasses(output.Passes),
	}
}

func protoShowCaptureCorrelation(correlation mitmshow.Correlation) *clydev1.ShowCaptureCorrelation {
	return &clydev1.ShowCaptureCorrelation{
		ClydeRequestId:    correlation.ClydeRequestID,
		CursorRequestId:   correlation.CursorRequestID,
		UpstreamRequestId: correlation.UpstreamRequestID,
		TraceId:           correlation.TraceID,
	}
}

func protoShowCapturePasses(passes []mitmshow.LookupPass) []*clydev1.ShowCapturePass {
	wirePasses := make([]*clydev1.ShowCapturePass, 0, len(passes))
	for _, pass := range passes {
		wirePasses = append(wirePasses, &clydev1.ShowCapturePass{
			Id:       pass.ID,
			Sections: protoShowCaptureSections(pass.Sections),
			Capture:  protoShowCaptureRows(pass.Capture),
			Found:    protoShowCaptureCorrelation(pass.Found),
		})
	}
	return wirePasses
}

func protoShowCaptureSections(sections []mitmshow.Section) []*clydev1.ShowCaptureSection {
	wireSections := make([]*clydev1.ShowCaptureSection, 0, len(sections))
	for _, section := range sections {
		wireSections = append(wireSections, &clydev1.ShowCaptureSection{
			Source:  section.Source,
			Path:    section.Path,
			Matches: section.Matches,
		})
	}
	return wireSections
}

func protoShowCaptureRows(capture mitmshow.CaptureSection) *clydev1.ShowCaptureRows {
	return &clydev1.ShowCaptureRows{
		Source: capture.Source,
		Path:   capture.Path,
		Rows:   protoShowCaptureCaptureRows(capture.Rows),
	}
}

func protoShowCaptureCaptureRows(rows []mitmshow.CaptureRow) []*clydev1.ShowCaptureCaptureRow {
	wireRows := make([]*clydev1.ShowCaptureCaptureRow, 0, len(rows))
	for _, row := range rows {
		wireRows = append(wireRows, &clydev1.ShowCaptureCaptureRow{
			Ts:                row.Timestamp,
			Client:            row.Client,
			Provider:          row.Provider,
			Concern:           row.Concern,
			Host:              row.Host,
			Method:            row.Method,
			Path:              row.Path,
			Status:            int64(row.Status),
			RequestId:         row.RequestID,
			UpstreamRequestId: row.UpstreamRequestID,
			SessionId:         row.SessionID,
			TraceId:           row.TraceID,
		})
	}
	return wireRows
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func protoConversationStats(stats conversation.Stats) *clydev1.ConversationInfoStats {
	return &clydev1.ConversationInfoStats{
		TotalMessages:     int64(stats.TotalMessages),
		VisibleMessages:   int64(stats.VisibleMessages),
		UserMessages:      int64(stats.UserMessages),
		AssistantMessages: int64(stats.AssistantMessages),
		SystemMessages:    int64(stats.SystemMessages),
		ToolCallCount:     int64(stats.ToolCallCount),
		ToolOutputCount:   int64(stats.ToolOutputCount),
	}
}

func protoConversationSegments(
	segments []conversation.CompactionSegment,
) []*clydev1.ConversationCompactionSegment {
	wireSegments := make([]*clydev1.ConversationCompactionSegment, 0, len(segments))
	for _, segment := range segments {
		summaryUnix := int64(0)
		if segment.HasStartingSummary {
			summaryUnix = segment.SummaryTimestamp.Unix()
		}
		wireSegments = append(wireSegments, &clydev1.ConversationCompactionSegment{
			Index:                int64(segment.Index),
			StartMessageIndex:    int64(segment.StartMessageIndex),
			EndMessageIndex:      int64(segment.EndMessageIndex),
			HasStartingSummary:   segment.HasStartingSummary,
			SummaryMessageIndex:  int64(segment.SummaryMessageIndex),
			SummaryUuid:          segment.SummaryUUID,
			SummaryTimestampUnix: summaryUnix,
			VisibleMessageCount:  int64(segment.VisibleMessageCount),
			ToolCallCount:        int64(segment.ToolCallCount),
			ExportSelector:       strconv.Itoa(segment.Index),
		})
	}
	return wireSegments
}
