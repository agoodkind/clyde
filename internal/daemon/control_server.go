package daemon

import (
	"context"
	"fmt"
	"log/slog"
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
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/response"
	searchstore "goodkind.io/clyde/internal/search/store"
	"goodkind.io/gklog/correlation"
)

const providerStatsStreamInterval = time.Second

type controlServer struct {
	clydev1.UnimplementedClydeServiceServer
	stats         *providerStatsRecorder
	index         *conversation.Index
	searchJobs    *conversation.SearchJobManager
	searchConfig  config.SearchConfig
	loggingConfig config.LoggingConfig
	mitmConfig    config.MITMConfig
	mitmStatus    func() MITMStatus
	showCapture   func(ctx context.Context, id string, asJSON bool) (string, error)
	reload        func(context.Context) (*clydev1.ReloadDaemonResponse, error)
	rebind        func(context.Context) (*clydev1.ReloadDaemonResponse, error)
	// semanticSearch is the engine-backed cross-conversation search the daemon
	// prefers before the live literal scan. It is nil when conversation semantic
	// search is not configured. semanticCollectionID names the engine collection
	// the search reads.
	semanticSearch       conversationSemanticSearchClient
	semanticCollectionID string
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

// maxConversationIDScope caps how many conversation ids are pushed into the
// engine as retrieval scope. Each id becomes one path-prefix clause in the
// vector store's filter expression, so an unbounded set would build an
// enormous expression; past the cap the engine searches unscoped and the
// record filter applies to the hits instead.
const maxConversationIDScope = 64

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

// GetConversation resolves one conversation and returns its transcript rendered
// as plain text.
func (s *controlServer) GetConversation(ctx context.Context, req *clydev1.GetConversationRequest) (*clydev1.GetConversationResponse, error) {
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.get_conversation.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return nil, status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	text, err := s.index.RenderPlainText(record, int(req.GetLastN()))
	if err != nil {
		slog.WarnContext(ctx, "daemon.get_conversation.render_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "render conversation: %v", err)
	}
	return &clydev1.GetConversationResponse{Text: text}, nil
}

// GetConversationContext resolves one conversation and returns the messages
// around a center point rendered as plain text.
func (s *controlServer) GetConversationContext(ctx context.Context, req *clydev1.GetConversationContextRequest) (*clydev1.GetConversationContextResponse, error) {
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.get_context.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return nil, status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	text, err := s.index.ContextWindowText(record, req.GetTimestamp(), int(req.GetMessageIndex()), int(req.GetBefore()), int(req.GetAfter()))
	if err != nil {
		slog.WarnContext(ctx, "daemon.get_context.render_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "render context: %v", err)
	}
	return &clydev1.GetConversationContextResponse{Text: text}, nil
}

// SearchConversations scans transcript text for candidate conversations and
// returns bounded first matches that an agent can pass to get or context.
func (s *controlServer) SearchConversations(ctx context.Context, req *clydev1.SearchConversationsRequest) (*clydev1.SearchConversationsResponse, error) {
	ctx, _ = correlation.Ensure(ctx, "")
	if req.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	result, err := searchConversationsResult(ctx, s.index, s.semanticSearch, s.semanticCollectionID, req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search conversations: %v", err)
	}
	return searchConversationsResponse(ctx, s.index, result), nil
}

// searchConversationsResult runs the engine-first cross-conversation search.
// When the engine is configured and produces at least one match, those matches
// are returned. Otherwise the live literal scan answers the request, and the
// Warming flag is set whenever the engine was configured but its result was
// empty or it failed.
func searchConversationsResult(
	ctx context.Context,
	idx searchConversationsIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	req *clydev1.SearchConversationsRequest,
) (conversation.SearchConversationsResult, error) {
	if semantic != nil {
		matches := engineSearchMatches(ctx, idx, semantic, collectionID, req)
		if len(matches) >= 1 {
			return conversation.SearchConversationsResult{
				Matches:              matches,
				ConversationsScanned: len(matches),
				ReturnedCount:        len(matches),
				Limit:                int(req.GetLimit()),
				HasMore:              false,
				Warming:              false,
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
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.live_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return conversation.SearchConversationsResult{}, fmt.Errorf("live search conversations: %w", err)
	}
	result.Warming = semantic != nil
	return result, nil
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
	filter := semsearch.SearchFilter{
		Roles:                req.GetRoles(),
		FromUnix:             req.GetFromUnix(),
		UntilUnix:            req.GetUntilUnix(),
		ConversationIDs:      scopeConversationIDs(ctx, idx, provider, workspace, includeArchived),
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
		if !conversation.RecordMatchesFilter(record, provider, workspace, includeArchived) {
			continue
		}
		matches = append(matches, conversation.SearchMatch{
			Record:       record,
			MessageIndex: int(hit.MessageIndex),
			Role:         hit.Role,
			Timestamp:    time.Unix(hit.TimestampUnix, 0),
			Snippet:      conversation.Snippet(hit.Content),
			Score:        hit.Score,
		})
		if limit > 0 && len(matches) >= limit {
			break
		}
	}
	return matches
}

// scopeConversationIDs resolves the request's record-level filters into the
// conversation-id set that scopes engine retrieval. It returns nil (no scope,
// post-filtering still applies) when no record filter is set, when resolution
// fails, or when the set is too large to push down as a filter expression.
func scopeConversationIDs(ctx context.Context, idx searchConversationsIndex, provider conversation.Provider, workspace string, includeArchived bool) []string {
	if !provider.Valid() && workspace == "" {
		return nil
	}
	conversationIDs, err := idx.ConversationIDsMatching(ctx, provider, workspace, includeArchived)
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.scope_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil
	}
	if len(conversationIDs) == 0 || len(conversationIDs) > maxConversationIDScope {
		return nil
	}
	return conversationIDs
}

// searchConversationsResponse maps the cross-conversation search result onto its
// wire response, carrying the Warming signal so the caller can show a live
// fallback note while the engine collection is cold.
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
		})
	}
	return &clydev1.SearchConversationsResponse{
		Matches:              matches,
		ConversationsScanned: int64(result.ConversationsScanned),
		ReturnedCount:        int64(result.ReturnedCount),
		Limit:                int64(result.Limit),
		HasMore:              result.HasMore,
		Warming:              result.Warming,
	}
}

// SearchConversation starts an async search job and returns its result id
// immediately. The caller polls GetSearchStatus for progress and the result.
func (s *controlServer) SearchConversation(ctx context.Context, req *clydev1.SearchConversationRequest) (*clydev1.SearchConversationResponse, error) {
	ctx, _ = correlation.Ensure(ctx, "")
	client, _ := peer.FromContext(ctx)
	if req.GetQuery() == "" {
		return &clydev1.SearchConversationResponse{Text: response.Text(ctx, "query is required")}, nil
	}
	resultID, err := s.searchJobs.Start(ctx, req.GetConversationId(), req.GetQuery(), conversation.WithinSearchOptions{
		Limit:     int(req.GetLimit()),
		Roles:     req.GetRoles(),
		FromUnix:  req.GetFromUnix(),
		UntilUnix: req.GetUntilUnix(),
		MinScore:  req.GetMinScore(),
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.search.start_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "start search: %v", err)
	}
	hint := fmt.Sprintf("Search started.\nresult_id: %s\nPoll with: conversation search status %s", resultID, resultID)
	return &clydev1.SearchConversationResponse{Text: response.Text(ctx, hint), ResultId: resultID}, nil
}

// GetSearchStatus returns the state, progress, and (when complete) the result of
// an async search job.
func (s *controlServer) GetSearchStatus(ctx context.Context, req *clydev1.GetSearchStatusRequest) (*clydev1.GetSearchStatusResponse, error) {
	ctx, _ = correlation.Ensure(ctx, "")
	if req.GetResultId() == "" {
		return nil, status.Error(codes.InvalidArgument, "result_id is required")
	}
	job, ok, err := s.searchJobs.Status(ctx, req.GetResultId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get search status: %v", err)
	}
	if !ok {
		return &clydev1.GetSearchStatusResponse{Found: false, ResultId: req.GetResultId()}, nil
	}
	return &clydev1.GetSearchStatusResponse{
		Found:    true,
		ResultId: job.ResultID,
		Status:   protoSearchStatus(job.Status),
		Progress: &clydev1.SearchProgress{
			ChunksDone:  int64(job.Progress.ChunksDone),
			ChunksTotal: int64(job.Progress.ChunksTotal),
			LayerIndex:  int64(job.Progress.LayerIndex),
			LayerTotal:  int64(job.Progress.LayerTotal),
			LayerName:   job.Progress.LayerName,
		},
		ResultText: job.ResultText,
		Error:      job.Error,
	}, nil
}

// CancelSearch cancels a running search job and reports its resulting status.
func (s *controlServer) CancelSearch(ctx context.Context, req *clydev1.CancelSearchRequest) (*clydev1.CancelSearchResponse, error) {
	ctx, _ = correlation.Ensure(ctx, "")
	if req.GetResultId() == "" {
		return nil, status.Error(codes.InvalidArgument, "result_id is required")
	}
	if err := s.searchJobs.Cancel(ctx, req.GetResultId()); err != nil {
		return nil, status.Errorf(codes.Internal, "cancel search: %v", err)
	}
	job, ok, err := s.searchJobs.Status(ctx, req.GetResultId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cancel search: %v", err)
	}
	if !ok {
		return &clydev1.CancelSearchResponse{Status: clydev1.SearchStatus_SEARCH_STATUS_UNSPECIFIED}, nil
	}
	return &clydev1.CancelSearchResponse{Status: protoSearchStatus(job.Status)}, nil
}

// AnalyzeSearchResults runs the configured analysis model over a completed
// search job's stored result set.
func (s *controlServer) AnalyzeSearchResults(ctx context.Context, req *clydev1.AnalyzeSearchResultsRequest) (*clydev1.AnalyzeSearchResultsResponse, error) {
	ctx, _ = correlation.Ensure(ctx, "")
	client, _ := peer.FromContext(ctx)
	if req.GetResultId() == "" || req.GetPrompt() == "" {
		return &clydev1.AnalyzeSearchResultsResponse{Text: response.Text(ctx, "result_id and prompt are required")}, nil
	}
	text, err := s.searchJobs.Analyze(ctx, req.GetResultId(), req.GetPrompt())
	if err != nil {
		slog.WarnContext(ctx, "daemon.analyze.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"result_id", req.GetResultId(),
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "analyze results: %v", err)
	}
	return &clydev1.AnalyzeSearchResultsResponse{Text: response.Text(ctx, text)}, nil
}

func protoSearchStatus(s searchstore.Status) clydev1.SearchStatus {
	switch s {
	case searchstore.StatusPending:
		return clydev1.SearchStatus_SEARCH_STATUS_PENDING
	case searchstore.StatusRunning:
		return clydev1.SearchStatus_SEARCH_STATUS_RUNNING
	case searchstore.StatusComplete:
		return clydev1.SearchStatus_SEARCH_STATUS_COMPLETE
	case searchstore.StatusFailed:
		return clydev1.SearchStatus_SEARCH_STATUS_FAILED
	case searchstore.StatusCanceled:
		return clydev1.SearchStatus_SEARCH_STATUS_CANCELED
	default:
		return clydev1.SearchStatus_SEARCH_STATUS_UNSPECIFIED
	}
}

// ExportTranscript resolves one conversation and returns its exported body.
func (s *controlServer) ExportTranscript(ctx context.Context, req *clydev1.ExportTranscriptRequest) (*clydev1.ExportTranscriptResponse, error) {
	client, _ := peer.FromContext(ctx)
	record, err := s.index.Resolve(ctx, req.GetConversationId())
	if err != nil {
		slog.WarnContext(ctx, "daemon.export.resolve_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", req.GetConversationId(),
			"err", err,
		)
		return nil, status.Errorf(codes.NotFound, "resolve conversation: %v", err)
	}
	options := conversation.ExportOptions{
		Format:       conversation.ExportFormat(req.GetFormat()),
		HistoryStart: int(req.GetHistoryStart()),
		Whitespace:   conversation.WhitespaceMode(req.GetWhitespace()),
		Content:      contentKindSetFromExportRequest(req),
	}
	body, err := s.index.Export(record, options)
	if err != nil {
		slog.WarnContext(ctx, "daemon.export.failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"peer", peerString(client),
			"conversation_id", record.ID,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "export transcript: %v", err)
	}
	return &clydev1.ExportTranscriptResponse{Body: body}, nil
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

// SeedBaseline extracts a v2 wire baseline from a capture transcript and writes
// reference-v2.toml for the given upstream.
func (s *controlServer) SeedBaseline(ctx context.Context, req *clydev1.SeedBaselineRequest) (*clydev1.SeedBaselineResponse, error) {
	result, err := mitm.SeedBaseline(ctx, req.GetFrom(), req.GetUpstream(), req.GetOutput(), req.GetIncludeUa(), req.GetExcludeUa())
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
		Written:  result.Written,
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
	case providerid.ProviderUnspecified:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	default:
		return clydev1.Provider_PROVIDER_UNSPECIFIED
	}
}
