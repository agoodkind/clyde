package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/loginventory"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
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
	showCapture   func(ctx context.Context, id string, asJSON bool) (string, error)
	reload        func(context.Context) (*clydev1.ReloadDaemonResponse, error)
	rebind        func(context.Context) (*clydev1.ReloadDaemonResponse, error)
	// freshness reports the conversation-index sync snapshot at query time, set
	// at construction. Nil yields the zero-value freshness.
	freshness func() conversation.SearchFreshness
	// semanticSearch is the engine-backed cross-conversation search the daemon
	// prefers before the live literal scan. It is nil when conversation semantic
	// search is not configured. semanticCollectionID names the engine collection
	// the search reads.
	semanticSearch       conversationSemanticSearchClient
	semanticCollectionID string
	// literalFallback re-enables the live literal scan when the engine is
	// configured but returns no matches. False (the default) makes a cold or
	// empty engine surface a loud LiteralDisabledCold result instead of a
	// misleading literal scan. It has no effect when semanticSearch is nil.
	literalFallback bool
	// captureStore is the daemon's shared SQLite capture store. SeedBaseline
	// reads the deduped shape corpus from it and writes the baseline through
	// it; nil when MITM is disabled.
	captureStore *capture.Store
}

// conversationSemanticSearchClient is the engine-backed conversation retrieval
// surface: the cross-conversation search the control server prefers before the
// live literal scan, and the within-conversation search whose fingerprint
// drives the literal-tail fallback. The semsearch client satisfies it; tests
// supply a fake.
type conversationSemanticSearchClient interface {
	SearchConversations(ctx context.Context, collectionID, query string, limit int32, filter semsearch.SearchFilter, perConversationLimit int32) ([]semsearch.SemHit, error)
	SearchWithinConversation(ctx context.Context, collectionID, conversationID, query string, limit int32, filter semsearch.SearchFilter) ([]semsearch.SemHit, string, error)
}

// searchConversationsIndex is the slice of the conversation index the
// engine-first cross-conversation search needs: exact-id record lookup for
// resolving engine hits, record-filter resolution into the id set that scopes
// engine retrieval, and the live literal scan used as the fallback.
type searchConversationsIndex interface {
	RecordByID(id string) (conversation.Record, bool)
	ConversationIDsMatching(ctx context.Context, provider conversation.Provider, workspaceRoot string, includeArchived bool) ([]string, error)
	SearchConversations(context.Context, conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error)
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
	ctx, _ = correlation.Ensure(ctx, "")
	if req.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	result, err := searchConversationsResult(ctx, s.index, s.semanticSearch, s.semanticCollectionID, s.literalFallback, req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search conversations: %v", err)
	}
	result.Freshness = s.freshnessSnapshot()
	return searchConversationsResponse(ctx, s.index, result), nil
}

// ReorientConversation resolves the recovery context for one conversation and
// returns one cursor-paged page of evidence. The page is bounded so it renders
// inline, and the caller loops on next_cursor until remaining is zero.
func (s *controlServer) ReorientConversation(ctx context.Context, req *clydev1.ReorientConversationRequest) (*clydev1.ReorientConversationResponse, error) {
	ctx, _ = correlation.Ensure(ctx, "")
	text, nextCursor, remaining, err := s.index.ReorientPageText(ctx, conversation.ReorientOptions{
		ConversationID: req.GetConversationId(),
		WorkspaceRoot:  req.GetWorkspace(),
		Topic:          req.GetTopic(),
		Before:         int(req.GetWindow()),
		After:          int(req.GetWindow()),
		Limit:          int(req.GetLimit()),
	}, req.GetCursor(), int(req.GetPageBytes()), req.GetJson())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reorient conversation: %v", err)
	}
	return &clydev1.ReorientConversationResponse{
		Text:       text,
		NextCursor: nextCursor,
		Remaining:  int64(remaining),
	}, nil
}

// freshnessSnapshot reads the conversation-index sync snapshot, returning the
// zero value when no freshness source is wired.
func (s *controlServer) freshnessSnapshot() conversation.SearchFreshness {
	if s.freshness == nil {
		return conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0}
	}
	return s.freshness()
}

// searchConversationsResult runs the engine-first cross-conversation search.
// When the engine is configured and produces at least one match, those matches
// are returned with Source semantic. When the engine is configured but produces
// nothing, the literalFallback flag decides: false (the dogfood default) returns
// a loud LiteralDisabledCold result with no matches, true falls through to the
// live literal scan. When the engine is not configured at all, the literal scan
// always answers. Facets and the filter funnel are computed clyde-side from the
// index. conversation_id, when set, scopes the engine to a single conversation,
// the within-search behavior.
func searchConversationsResult(
	ctx context.Context,
	idx searchConversationsIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	literalFallback bool,
	req *clydev1.SearchConversationsRequest,
) (conversation.SearchConversationsResult, error) {
	accounting := filterAccounting(ctx, idx, req)
	if semantic != nil {
		matches := engineSearchMatches(ctx, idx, semantic, collectionID, req)
		if len(matches) >= 1 {
			return conversation.SearchConversationsResult{
				Matches:              matches,
				ConversationsScanned: len(matches),
				ReturnedCount:        len(matches),
				Limit:                int(req.GetLimit()),
				HasMore:              false,
				Source:               conversation.SearchSourceSemantic,
				Facets:               conversation.ComputeFacets(matches, searchFacetTopN),
				Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
				FilterAccounting:     appendReturnedStage(accounting, len(matches)),
			}, nil
		}
		if !literalFallback {
			// Dogfood: the engine is configured but cold or empty and the literal
			// crutch is off, so return an explicit empty result the caller can
			// branch on instead of a misleading literal scan.
			slog.WarnContext(ctx, "daemon.search_conversations.literal_disabled_cold", "concern", "process.daemon.lifecycle", "component", "daemon",
				"query", req.GetQuery(),
			)
			return conversation.SearchConversationsResult{
				Matches:              nil,
				ConversationsScanned: 0,
				ReturnedCount:        0,
				Limit:                int(req.GetLimit()),
				HasMore:              false,
				Source:               conversation.SearchSourceLiteralDisabledCold,
				Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
				Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
				FilterAccounting:     appendReturnedStage(accounting, 0),
			}, nil
		}
	}
	result, err := idx.SearchConversations(ctx, conversation.SearchConversationsOptions{
		Query:                req.GetQuery(),
		Limit:                int(req.GetLimit()),
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
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.live_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return conversation.SearchConversationsResult{}, fmt.Errorf("live search conversations: %w", err)
	}
	result.Source = conversation.SearchSourceLiteral
	result.Facets = conversation.ComputeFacets(result.Matches, searchFacetTopN)
	result.FilterAccounting = appendReturnedStage(accounting, len(result.Matches))
	return result, nil
}

// searchFacetTopN bounds each facet dimension to its top values by count.
const searchFacetTopN = 5

// filterAccounting builds the ordered candidate-count funnel from the index:
// the indexed baseline first, then one stage per active filter, computed by
// reusing ConversationIDsMatching. A stage whose count cannot be resolved is
// omitted rather than fabricated; the indexed baseline and the caller-appended
// returned stage keep the funnel honest.
func filterAccounting(ctx context.Context, idx searchConversationsIndex, req *clydev1.SearchConversationsRequest) []conversation.FilterStage {
	var anyProvider conversation.Provider
	stages := make([]conversation.FilterStage, 0, 5)
	if all, err := idx.ConversationIDsMatching(ctx, anyProvider, "", true); err == nil {
		stages = append(stages, conversation.FilterStage{Name: "indexed", Remaining: len(all)})
	}
	provider := providerFromProto(req.GetProvider())
	if provider.Valid() {
		if matched, err := idx.ConversationIDsMatching(ctx, provider, "", true); err == nil {
			stages = append(stages, conversation.FilterStage{Name: "provider", Remaining: len(matched)})
		}
	}
	if req.GetWorkspace() != "" {
		if matched, err := idx.ConversationIDsMatching(ctx, provider, req.GetWorkspace(), true); err == nil {
			stages = append(stages, conversation.FilterStage{Name: "workspace", Remaining: len(matched)})
		}
	}
	if !req.GetIncludeArchived() {
		if matched, err := idx.ConversationIDsMatching(ctx, provider, req.GetWorkspace(), false); err == nil {
			stages = append(stages, conversation.FilterStage{Name: "archived_excluded", Remaining: len(matched)})
		}
	}
	if req.GetConversationId() != "" {
		stages = append(stages, conversation.FilterStage{Name: "conversation", Remaining: 1})
	}
	return stages
}

// appendReturnedStage closes the funnel with the count actually returned.
func appendReturnedStage(stages []conversation.FilterStage, returned int) []conversation.FilterStage {
	return append(stages, conversation.FilterStage{Name: "returned", Remaining: returned})
}

// engineSearchMatches resolves each engine hit to a cached record, skipping
// hits with no record or that fail the request provider, workspace, or archived
// filter, and returns the bounded matches. A nil result (engine error or no
// usable hits) signals the caller to fall back to the live literal scan.
func engineSearchMatches(
	ctx context.Context,
	idx searchConversationsIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	req *clydev1.SearchConversationsRequest,
) []conversation.SearchMatch {
	limit := int(req.GetLimit())
	provider := providerFromProto(req.GetProvider())
	workspace := req.GetWorkspace()
	includeArchived := req.GetIncludeArchived()

	var providers []string
	if provider.Valid() {
		providers = []string{provider.String()}
	}
	var conversationIDs []string
	switch {
	case req.GetConversationId() != "":
		// A conversation_id scopes the engine to that one conversation, the
		// within-search behavior. Provider and workspace scope are irrelevant
		// then but still safe to pass; the id set is the tightest filter.
		conversationIDs = []string{req.GetConversationId()}
	case workspace != "":
		conversationIDs = workspaceConversationIDs(ctx, idx, workspace)
		if len(conversationIDs) == 0 {
			// The workspace matched no conversations, so the result is empty. An
			// empty id set would otherwise read as "no scope" and match the whole
			// corpus, so short-circuit instead of calling the engine.
			return []conversation.SearchMatch{}
		}
	}
	filter := semsearch.SearchFilter{
		Providers:            providers,
		WorkspaceRoots:       nil,
		Roles:                req.GetRoles(),
		FromUnix:             req.GetFromUnix(),
		UntilUnix:            req.GetUntilUnix(),
		ConversationIDs:      conversationIDs,
		ParentConversationID: "",
		MinScore:             req.GetMinScore(),
		MessageIndexFrom:     0,
		MessageIndexUntil:    0,
	}
	hits, err := semantic.SearchConversations(ctx, collectionID, req.GetQuery(), int32FromInt(limit), filter, int32FromInt(int(req.GetPerConversationLimit())))
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.engine_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil
	}
	matches := make([]conversation.SearchMatch, 0, len(hits))
	for _, hit := range hits {
		record, ok := idx.RecordByID(hit.ConversationID)
		if !ok {
			continue
		}
		// provider is filtered natively by the engine; workspace is filtered via
		// the resolved conversation-id set above, not the native workspace column,
		// which is null on rows indexed before it existed. archived is a clyde
		// record flag not stored on the engine rows, so it stays a clyde-side
		// check. Archived conversations are rare, so dropping one from the top-K
		// has negligible effect on recall.
		if record.Archived && !includeArchived {
			continue
		}
		matches = append(matches, conversation.SearchMatch{
			Record:       record,
			MessageIndex: int(hit.MessageIndex),
			Role:         hit.Role,
			Timestamp:    time.Unix(hit.TimestampUnix, 0),
			Snippet:      conversation.Snippet(hit.Content),
			Score:        hit.Score,
			// The excerpt is the matched passage the engine already returned,
			// byte-bounded so the ranked list stays small. The full surrounding
			// window is a separate windowed read; search never inlines it.
			ContextWindow: conversation.Excerpt(hit.Content),
		})
		if limit > 0 && len(matches) >= limit {
			break
		}
	}
	return matches
}

// workspaceConversationIDs resolves a workspace filter to the conversation-id
// set under it (prefix match, so a project root includes its subdirectories),
// regardless of provider or archived state, which are handled separately. The
// engine filters natively on the conversation_id column and batches a large
// set. Returns nil on resolution failure.
func workspaceConversationIDs(ctx context.Context, idx searchConversationsIndex, workspace string) []string {
	var anyProvider conversation.Provider
	conversationIDs, err := idx.ConversationIDsMatching(ctx, anyProvider, workspace, true)
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.scope_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil
	}
	return conversationIDs
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
	case conversation.SearchSourceLiteral:
		return clydev1.SearchSource_SEARCH_SOURCE_LITERAL
	case conversation.SearchSourceLiteralDisabledCold:
		return clydev1.SearchSource_SEARCH_SOURCE_LITERAL_DISABLED_COLD
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
// capture store and returns the rendered text or JSON document.
func (s *controlServer) ShowCapture(ctx context.Context, req *clydev1.ShowCaptureRequest) (*clydev1.ShowCaptureResponse, error) {
	if s.showCapture == nil {
		return &clydev1.ShowCaptureResponse{Output: ""}, nil
	}
	output, err := s.showCapture(ctx, req.GetId(), req.GetJson())
	if err != nil {
		client, _ := peer.FromContext(ctx)
		slog.WarnContext(ctx, "daemon.show_capture.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "show capture: %v", err)
	}
	return &clydev1.ShowCaptureResponse{Output: output}, nil
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
// returns it rendered as a table or as JSON.
func (s *controlServer) LogsInventory(ctx context.Context, req *clydev1.LogsInventoryRequest) (*clydev1.LogsInventoryResponse, error) {
	output, err := loginventory.Render(
		req.GetStateRoot(),
		int(req.GetLargestFileLimit()),
		req.GetDeep(),
		s.loggingConfig,
		s.mitmConfig,
		req.GetJson(),
	)
	if err != nil {
		client, _ := peer.FromContext(ctx)
		slog.WarnContext(ctx, "daemon.logs_inventory.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "logs inventory: %v", err)
	}
	return &clydev1.LogsInventoryResponse{Output: output}, nil
}

func (s *controlServer) SubscribeProviderStats(_ *clydev1.SubscribeProviderStatsRequest, stream grpc.ServerStreamingServer[clydev1.ProviderStatsEvent]) error {
	ticker := time.NewTicker(providerStatsStreamInterval)
	defer ticker.Stop()
	for {
		response := providerStatsResponse(s.stats)
		for _, stats := range response.GetProviders() {
			event := &clydev1.ProviderStatsEvent{
				Stats:         stats,
				EmittedAtUnix: response.GetLoadedAtUnix(),
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
		case <-ticker.C:
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
	const maxInt32Value = 2147483647
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
		Id:            record.ID,
		Provider:      protoProvider(record.Provider),
		NativeId:      record.NativeID,
		Title:         record.Title,
		WorkspaceRoot: record.WorkspaceRoot,
		ArtifactPath:  record.ArtifactPath,
		ArtifactKind:  record.ArtifactKind,
		Model:         record.Model,
		CreatedAtUnix: record.CreatedAt.Unix(),
		UpdatedAtUnix: record.UpdatedAt.Unix(),
		SizeBytes:     record.SizeBytes,
		Archived:      record.Archived,
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

func providerFromProto(provider clydev1.Provider) providerid.Provider {
	switch provider {
	case clydev1.Provider_PROVIDER_CLAUDE:
		return providerid.ProviderClaude
	case clydev1.Provider_PROVIDER_CODEX:
		return providerid.ProviderCodex
	case clydev1.Provider_PROVIDER_ANTHROPIC:
		return providerid.ProviderAnthropic
	case clydev1.Provider_PROVIDER_OPENAI_COMPAT:
		return providerid.ProviderOpenAICompat
	case clydev1.Provider_PROVIDER_MITM:
		return providerid.ProviderMITM
	case clydev1.Provider_PROVIDER_ARTIFACT:
		return providerid.ProviderArtifact
	case clydev1.Provider_PROVIDER_CURSOR:
		return providerid.ProviderCursor
	case clydev1.Provider_PROVIDER_UNSPECIFIED:
		return providerid.ProviderUnspecified
	default:
		return providerid.ProviderUnspecified
	}
}

func protoProvider(provider providerid.Provider) clydev1.Provider {
	switch provider {
	case providerid.ProviderClaude:
		return clydev1.Provider_PROVIDER_CLAUDE
	case providerid.ProviderCodex:
		return clydev1.Provider_PROVIDER_CODEX
	case providerid.ProviderAnthropic:
		return clydev1.Provider_PROVIDER_ANTHROPIC
	case providerid.ProviderOpenAICompat:
		return clydev1.Provider_PROVIDER_OPENAI_COMPAT
	case providerid.ProviderMITM:
		return clydev1.Provider_PROVIDER_MITM
	case providerid.ProviderArtifact:
		return clydev1.Provider_PROVIDER_ARTIFACT
	case providerid.ProviderCursor:
		return clydev1.Provider_PROVIDER_CURSOR
	case providerid.ProviderConductor:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	case providerid.ProviderUnspecified:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	default:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	}
}
