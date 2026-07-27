package daemon

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSearchIndex stands in for *conversation.Index in source tests.
type fakeSearchIndex struct {
	records     map[string]conversation.Record
	matchingErr error
}

func (f *fakeSearchIndex) RecordByID(id string) (conversation.Record, bool) {
	record, ok := f.records[id]
	return record, ok
}

func (f *fakeSearchIndex) ConversationIDsMatching(_ context.Context, provider conversation.Provider, workspaceRoot string, includeArchived bool) ([]string, error) {
	if f.matchingErr != nil {
		return nil, f.matchingErr
	}
	conversationIDs := make([]string, 0, len(f.records))
	for id, record := range f.records {
		if !includeArchived && record.Archived {
			continue
		}
		if provider.Valid() && record.Provider != provider {
			continue
		}
		if workspaceRoot != "" && record.WorkspaceRoot != workspaceRoot && !strings.HasPrefix(record.WorkspaceRoot, workspaceRoot+"/") {
			continue
		}
		conversationIDs = append(conversationIDs, id)
	}
	return conversationIDs, nil
}

// fakeSemanticSearch is a canned engine-backed search client. It records the
// filter each across-search received so tests can prove id-set pushdown.
type fakeSemanticSearch struct {
	hits    []semsearch.SemHit
	err     error
	filters []semsearch.SearchFilter
	limits  []int32
}

// SearchConversations truncates to the requested limit the way a real ranked
// engine does, so a test can distinguish a page the engine filled from one it
// exhausted.
func (f *fakeSemanticSearch) SearchConversations(_ context.Context, _ string, _ string, limit int32, filter semsearch.SearchFilter, _ int32) ([]semsearch.SemHit, error) {
	f.filters = append(f.filters, filter)
	f.limits = append(f.limits, limit)
	if f.err != nil {
		return nil, f.err
	}
	if limit > 0 && int(limit) < len(f.hits) {
		return f.hits[:limit], nil
	}
	return f.hits, nil
}

func (f *fakeSemanticSearch) SearchWithinConversation(context.Context, string, string, string, int32, semsearch.SearchFilter) ([]semsearch.SemHit, string, error) {
	return f.hits, "", f.err
}

func searchSemanticSource(
	ctx context.Context,
	index conversationSearchIndex,
	semantic conversationSemanticSearchClient,
	request *clydev1.SearchConversationsRequest,
) (conversation.SearchConversationsResult, error) {
	source := &semanticConversationSearchSource{
		index: index,
		searchClient: func() conversationSemanticSearchClient {
			return semantic
		},
		collectionID: "conversations",
	}
	return source.SearchConversations(ctx, searchConversationsOptionsFromProto(request))
}

type alternateConversationSearchSource struct {
	result  conversation.SearchConversationsResult
	options conversation.SearchConversationsOptions
}

func (s *alternateConversationSearchSource) SearchConversations(
	_ context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	s.options = options
	return s.result, nil
}

func daemonTestRecord(id string, archived bool) conversation.Record {
	return conversation.Record{
		ID:            id,
		Provider:      conversation.ProviderClaude,
		NativeID:      id,
		Lineage:       nil,
		Title:         id,
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/" + id + ".jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      archived,
	}
}

func TestControlServerSearchConversationsAcceptsAlternateSource(t *testing.T) {
	t.Parallel()
	record := daemonTestRecord("claude:alternate", false)
	source := &alternateConversationSearchSource{
		result: conversation.SearchConversationsResult{
			Matches: []conversation.SearchMatch{
				{
					Record:        record,
					MessageIndex:  4,
					Role:          "assistant",
					Timestamp:     time.Unix(8, 0),
					Snippet:       "alternate source match",
					Score:         0.75,
					ContextWindow: "alternate source match",
				},
			},
			ConversationsScanned: 1,
			ReturnedCount:        1,
			Limit:                7,
			Offset:               0,
			NextOffset:           1,
			HasMore:              false,
			Source:               conversation.SearchSourceUnspecified,
			Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
			Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
			FilterAccounting:     nil,
		},
		options: conversation.SearchConversationsOptions{},
	}
	server := &controlServer{
		index:        conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{}),
		searchSource: source,
	}

	response, err := server.SearchConversations(context.Background(), &clydev1.SearchConversationsRequest{
		Query:     "alternate",
		Limit:     7,
		Workspace: "/repo",
		Roles:     []string{"assistant"},
	})
	if err != nil {
		t.Fatalf("search conversations: %v", err)
	}
	if source.options.Query != "alternate" || source.options.Limit != 7 ||
		source.options.WorkspaceRoot != "/repo" || len(source.options.Roles) != 1 {
		t.Fatalf("source options = %+v", source.options)
	}
	if len(response.GetMatches()) != 1 ||
		response.GetMatches()[0].GetConversation().GetId() != record.ID {
		t.Fatalf("response matches = %+v", response.GetMatches())
	}
}

func TestSemanticConversationSearchSourceReturnsEngineHits(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:one":      daemonTestRecord("claude:one", false),
			"claude:archived": daemonTestRecord("claude:archived", true),
		},
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:missing", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "no record"},
			{ConversationID: "claude:archived", MessageIndex: 1, Role: "user", TimestampUnix: 6, Content: "archived"},
			{ConversationID: "claude:one", MessageIndex: 2, Role: "assistant", TimestampUnix: 7, Content: "the auth timeout note"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if result.Source != conversation.SearchSourceSemantic {
		t.Fatalf("source = %v, want semantic when the engine produced a match", result.Source)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("matches = %d, want 1 (missing record and archived skipped)", len(result.Matches))
	}
	match := result.Matches[0]
	if match.Record.ID != "claude:one" {
		t.Fatalf("match record = %q, want claude:one", match.Record.ID)
	}
	if match.MessageIndex != 2 || match.Role != "assistant" {
		t.Fatalf("match position = %d/%s, want 2/assistant", match.MessageIndex, match.Role)
	}
	if match.Snippet != "the auth timeout note" {
		t.Fatalf("snippet = %q, want %q", match.Snippet, "the auth timeout note")
	}
	if !match.Timestamp.Equal(time.Unix(7, 0)) {
		t.Fatalf("timestamp = %v, want %v", match.Timestamp, time.Unix(7, 0))
	}
}

func TestSearchConversationsResultEngineAppliesOffset(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:one": daemonTestRecord("claude:one", false),
			"claude:two": daemonTestRecord("claude:two", false),
		},
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "first auth timeout"},
			{ConversationID: "claude:two", MessageIndex: 1, Role: "assistant", TimestampUnix: 6, Content: "second auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 1, Offset: 1}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if len(semantic.limits) != 1 || semantic.limits[0] != 2 {
		t.Fatalf("engine limits = %v, want [2]", semantic.limits)
	}
	if result.Offset != 1 || result.NextOffset != 2 {
		t.Fatalf("offsets = %d/%d, want 1/2", result.Offset, result.NextOffset)
	}
	if len(result.Matches) != 1 || result.Matches[0].Record.ID != "claude:two" {
		t.Fatalf("matches = %+v, want claude:two", result.Matches)
	}
}

func TestSearchConversationsResultEngineNormalizesLimit(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:one": daemonTestRecord("claude:one", false),
			"claude:two": daemonTestRecord("claude:two", false),
		},
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "first auth timeout"},
			{ConversationID: "claude:two", MessageIndex: 1, Role: "assistant", TimestampUnix: 6, Content: "second auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 0, Offset: 1}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if len(semantic.limits) != 1 || semantic.limits[0] != int32(conversation.DefaultSearchLimit+1) {
		t.Fatalf("engine limits = %v, want [%d]", semantic.limits, conversation.DefaultSearchLimit+1)
	}
	if result.Limit != conversation.DefaultSearchLimit {
		t.Fatalf("limit = %d, want %d", result.Limit, conversation.DefaultSearchLimit)
	}
	if result.Offset != 1 || result.NextOffset != 2 {
		t.Fatalf("offsets = %d/%d, want 1/2", result.Offset, result.NextOffset)
	}
}

func TestSearchConversationsResultEngineNormalizesNegativeOffset(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:one": daemonTestRecord("claude:one", false),
		},
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 1, Offset: -5}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if len(semantic.limits) != 1 || semantic.limits[0] != 1 {
		t.Fatalf("engine limits = %v, want [1]", semantic.limits)
	}
	if result.Offset != 0 || result.NextOffset != 1 {
		t.Fatalf("offsets = %d/%d, want 0/1", result.Offset, result.NextOffset)
	}
}

func TestSearchConversationsResultEngineRejectsOffsetTooLarge(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
	}
	semantic := &fakeSemanticSearch{
		hits: nil,
		err:  nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10, Offset: int64(maxInt32Value)}

	_, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err == nil {
		t.Fatal("expected an error for an offset that overflows the semantic page size")
	}
	if !strings.Contains(err.Error(), "offset too large") {
		t.Fatalf("error = %q, want offset too large", err.Error())
	}
}

func TestSearchConversationsResultEngineEmptyPageStaysSemantic(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:one": daemonTestRecord("claude:one", false),
		},
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 1, Offset: 1}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if result.Source != conversation.SearchSourceSemantic {
		t.Fatalf("source = %v, want semantic", result.Source)
	}
	if result.ReturnedCount != 0 || result.Offset != 1 || result.NextOffset != 1 {
		t.Fatalf("result = %+v, want an empty semantic page at offset 1", result)
	}
}

func TestSemanticConversationSearchSourceReturnsSuccessfulEmptyResult(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
	}
	semantic := &fakeSemanticSearch{hits: nil, err: nil, filters: nil}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if result.Source != conversation.SearchSourceSemantic {
		t.Fatalf("source = %v, want semantic", result.Source)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("matches = %d, want 0", len(result.Matches))
	}
}

// TestSearchConversationsResultNativeFilters proves a provider-scoped request
// filters natively on the provider column (not a resolved id set), a
// workspace-scoped request resolves to the conversation-id set under that
// workspace, and an unscoped request pushes neither.
func TestSearchConversationsResultNativeFilters(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:one": daemonTestRecord("claude:one", false),
		},
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "hit", Score: 0.5, ParentConversationID: ""},
		},
		err:     nil,
		filters: nil,
	}
	providerScoped := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10, Provider: clydev1.Provider_PROVIDER_CLAUDE}

	if _, err := searchSemanticSource(context.Background(), idx, semantic, providerScoped); err != nil {
		t.Fatalf("provider-scoped search: %v", err)
	}
	if len(semantic.filters) != 1 {
		t.Fatalf("engine calls = %d, want 1", len(semantic.filters))
	}
	providerFilter := semantic.filters[0]
	if len(providerFilter.Providers) != 1 || providerFilter.Providers[0] != "claude" {
		t.Fatalf("provider filter providers = %v, want [claude]", providerFilter.Providers)
	}
	if len(providerFilter.ConversationIDs) != 0 {
		t.Fatalf("provider filter ids = %v, want none (provider is native)", providerFilter.ConversationIDs)
	}

	workspaceScoped := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10, Workspace: "/repo"}
	if _, err := searchSemanticSource(context.Background(), idx, semantic, workspaceScoped); err != nil {
		t.Fatalf("workspace-scoped search: %v", err)
	}
	if len(semantic.filters) != 2 {
		t.Fatalf("engine calls = %d, want 2", len(semantic.filters))
	}
	workspaceFilter := semantic.filters[1]
	if len(workspaceFilter.ConversationIDs) != 1 || workspaceFilter.ConversationIDs[0] != "claude:one" {
		t.Fatalf("workspace filter ids = %v, want [claude:one]", workspaceFilter.ConversationIDs)
	}

	unscoped := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}
	if _, err := searchSemanticSource(context.Background(), idx, semantic, unscoped); err != nil {
		t.Fatalf("unscoped search: %v", err)
	}
	if len(semantic.filters) != 3 {
		t.Fatalf("engine calls = %d, want 3", len(semantic.filters))
	}
	if len(semantic.filters[2].ConversationIDs) != 0 || len(semantic.filters[2].Providers) != 0 {
		t.Fatalf("unscoped filter = %+v, want no provider or id scope", semantic.filters[2])
	}
}

func TestSearchConversationsResultNativeFiltersSupportZedProvider(t *testing.T) {
	t.Parallel()

	record := daemonTestRecord("zed:one", false)
	record.Provider = conversation.ProviderZed
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"zed:one": record,
		},
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "zed:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "hit", Score: 0.5, ParentConversationID: ""},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10, Provider: clydev1.Provider_PROVIDER_ZED}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("zed provider search: %v", err)
	}
	if len(semantic.filters) != 1 {
		t.Fatalf("engine calls = %d, want 1", len(semantic.filters))
	}
	if len(semantic.filters[0].Providers) != 1 || semantic.filters[0].Providers[0] != "zed" {
		t.Fatalf("provider filter providers = %v, want [zed]", semantic.filters[0].Providers)
	}
	if len(semantic.filters[0].ConversationIDs) != 0 {
		t.Fatalf("provider filter ids = %v, want none for native zed filtering", semantic.filters[0].ConversationIDs)
	}
	if len(result.Matches) != 1 || result.Matches[0].Record.Provider != conversation.ProviderZed {
		t.Fatalf("matches = %+v", result.Matches)
	}
}

func TestSemanticConversationSearchSourceReportsUnavailableClient(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	_, err := searchSemanticSource(context.Background(), idx, nil, req)
	if err == nil {
		t.Fatal("expected a typed error when the engine is configured but unreachable")
	}
	var failure conversationSearchSourceError
	if !errors.As(err, &failure) || failure.code != conversationSearchSourceUnavailable {
		t.Fatalf("error = %v, want unavailable conversationSearchSourceError", err)
	}
}

func TestSemanticConversationSearchSourceReportsTransportFailure(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
	}
	semantic := &fakeSemanticSearch{hits: nil, err: status.Error(codes.Unavailable, "connection refused")}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	_, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err == nil {
		t.Fatal("expected a typed error on an engine transport failure")
	}
	var failure conversationSearchSourceError
	if !errors.As(err, &failure) || failure.code != conversationSearchSourceUnavailable {
		t.Fatalf("error = %v, want unavailable conversationSearchSourceError", err)
	}
}

// hiddenAndVisibleHits builds a ranked list whose first hiddenCount hits belong
// to conversations the index does not serve, followed by visibleCount hits that
// do resolve. It mirrors the live corpus, where retained subagent chunks stay
// rankable and can occupy the top of any page.
func hiddenAndVisibleHits(hiddenCount int, visibleCount int) ([]semsearch.SemHit, map[string]conversation.Record) {
	hits := make([]semsearch.SemHit, 0, hiddenCount+visibleCount)
	for i := 0; i < hiddenCount; i++ {
		hits = append(hits, semsearch.SemHit{
			ConversationID: "claude:hidden-" + strconv.Itoa(i),
			MessageIndex:   int32(i),
			Role:           "user",
			TimestampUnix:  int64(i),
			Content:        "hidden subagent chunk",
		})
	}
	records := make(map[string]conversation.Record, visibleCount)
	for i := 0; i < visibleCount; i++ {
		id := "claude:visible-" + strconv.Itoa(i)
		records[id] = daemonTestRecord(id, false)
		hits = append(hits, semsearch.SemHit{
			ConversationID: id,
			MessageIndex:   int32(i),
			Role:           "assistant",
			TimestampUnix:  int64(hiddenCount + i),
			Content:        "the milvus checkpoint note",
		})
	}
	return hits, records
}

func filterStageRemaining(stages []conversation.FilterStage, name string) (int, bool) {
	for _, stage := range stages {
		if stage.Name == name {
			return stage.Remaining, true
		}
	}
	return 0, false
}

// TestSearchConversationsResultOverfetchesPastHiddenConversations proves a page
// whose top ranked hits belong to hidden conversations still comes back full.
// Hidden rows stay in the vector store under the retain reconcile mode, so they
// keep consuming ranked slots that clyde then drops; without headroom the caller
// gets a short page and no signal that anything was withheld.
func TestSearchConversationsResultOverfetchesPastHiddenConversations(t *testing.T) {
	t.Parallel()
	hits, records := hiddenAndVisibleHits(3, 10)
	idx := &fakeSearchIndex{records: records}
	semantic := &fakeSemanticSearch{hits: hits, err: nil}
	req := &clydev1.SearchConversationsRequest{Query: "milvus checkpoint", Limit: 10}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if len(result.Matches) != 10 {
		t.Fatalf("matches = %d, want a full page of 10; hidden conversations consumed ranked slots", len(result.Matches))
	}
	if len(semantic.limits) < 2 {
		t.Fatalf("engine limits = %v, want a second, larger query once hidden hits shortened the page", semantic.limits)
	}
	if semantic.limits[1] <= semantic.limits[0] {
		t.Fatalf("engine limits = %v, want the retry to ask for more than the first attempt", semantic.limits)
	}
}

// TestSearchConversationsResultReportsWithheldHitsOnAShortPage covers the case
// the retry budget cannot rescue: hidden rows dominate the ranking deeper than
// the bounded over-fetch reaches. The page is short, so it must not claim to be
// complete, and the funnel must show that ranked hits were withheld.
func TestSearchConversationsResultReportsWithheldHitsOnAShortPage(t *testing.T) {
	t.Parallel()
	visible, records := hiddenAndVisibleHits(0, 2)
	hidden, _ := hiddenAndVisibleHits(600, 0)
	hits := append(visible, hidden...)
	idx := &fakeSearchIndex{records: records}
	semantic := &fakeSemanticSearch{hits: hits, err: nil}
	req := &clydev1.SearchConversationsRequest{Query: "milvus checkpoint", Limit: 10}

	result, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if len(result.Matches) != 2 {
		t.Fatalf("matches = %d, want the 2 that resolve", len(result.Matches))
	}
	if !result.HasMore {
		t.Fatal("HasMore = false on a short page the over-fetch budget could not fill, so the caller cannot tell ranked matches were dropped")
	}
	ranked, ok := filterStageRemaining(result.FilterAccounting, "engine_ranked")
	if !ok {
		t.Fatalf("filter accounting = %+v, want an engine_ranked stage", result.FilterAccounting)
	}
	visibleRemaining, ok := filterStageRemaining(result.FilterAccounting, "hidden_excluded")
	if !ok {
		t.Fatalf("filter accounting = %+v, want a hidden_excluded stage", result.FilterAccounting)
	}
	if ranked <= visibleRemaining {
		t.Fatalf("engine_ranked = %d, hidden_excluded = %d, want the funnel to show the withheld hits", ranked, visibleRemaining)
	}
}

func TestSearchConversationsResultWorkspaceScopeFailureReturnsError(t *testing.T) {
	t.Parallel()
	matchingErr := errors.New("read conversation index failed")
	idx := &fakeSearchIndex{records: nil, matchingErr: matchingErr}
	semantic := &fakeSemanticSearch{hits: nil, err: nil}
	req := &clydev1.SearchConversationsRequest{
		Query:     "auth",
		Limit:     10,
		Workspace: "/repo",
	}

	_, err := searchSemanticSource(context.Background(), idx, semantic, req)
	if err == nil {
		t.Fatal("expected workspace scope resolution failure")
	}
	var failure conversationSearchSourceError
	if !errors.As(err, &failure) || failure.code != conversationSearchSourceFailed {
		t.Fatalf("error = %v, want failed conversationSearchSourceError", err)
	}
	if !errors.Is(err, matchingErr) {
		t.Fatalf("error = %v, want wrapped conversation index failure", err)
	}
}
