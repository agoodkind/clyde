package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
)

// fakeSearchIndex stands in for *conversation.Index in engine-first search
// tests, exposing exact-id lookup and a canned live literal scan.
type fakeSearchIndex struct {
	records   map[string]conversation.Record
	live      conversation.SearchConversationsResult
	liveErr   error
	liveCalls int
	lastLive  conversation.SearchConversationsOptions
}

func (f *fakeSearchIndex) RecordByID(id string) (conversation.Record, bool) {
	record, ok := f.records[id]
	return record, ok
}

func (f *fakeSearchIndex) ConversationIDsMatching(_ context.Context, provider conversation.Provider, workspaceRoot string, includeArchived bool) ([]string, error) {
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

func (f *fakeSearchIndex) SearchConversations(_ context.Context, options conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error) {
	f.liveCalls++
	f.lastLive = options
	return f.live, f.liveErr
}

// fakeSemanticSearch is a canned engine-backed search client. It records the
// filter each across-search received so tests can prove id-set pushdown.
type fakeSemanticSearch struct {
	hits    []semsearch.SemHit
	err     error
	filters []semsearch.SearchFilter
	limits  []int32
}

func (f *fakeSemanticSearch) SearchConversations(_ context.Context, _ string, _ string, limit int32, filter semsearch.SearchFilter, _ int32) ([]semsearch.SemHit, error) {
	f.filters = append(f.filters, filter)
	f.limits = append(f.limits, limit)
	return f.hits, f.err
}

func (f *fakeSemanticSearch) SearchWithinConversation(context.Context, string, string, string, int32, semsearch.SearchFilter) ([]semsearch.SemHit, string, error) {
	return f.hits, "", f.err
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

func TestSearchConversationsResultEngineHitsReturnWithoutFallback(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:one":      daemonTestRecord("claude:one", false),
			"claude:archived": daemonTestRecord("claude:archived", true),
		},
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
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

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 0 {
		t.Fatalf("live scan called %d times, want 0 on engine hit", idx.liveCalls)
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
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "first auth timeout"},
			{ConversationID: "claude:two", MessageIndex: 1, Role: "assistant", TimestampUnix: 6, Content: "second auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 1, Offset: 1}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, req)
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
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "first auth timeout"},
			{ConversationID: "claude:two", MessageIndex: 1, Role: "assistant", TimestampUnix: 6, Content: "second auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 0, Offset: 1}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, req)
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
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 1, Offset: -5}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, req)
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
		records:   map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	semantic := &fakeSemanticSearch{
		hits: nil,
		err:  nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10, Offset: int64(maxInt32Value)}

	_, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, req)
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
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "auth timeout"},
		},
		err: nil,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 1, Offset: 1}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", true, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 0 {
		t.Fatalf("live scan called %d times, want 0 when semantic results exist but the requested page is past the end", idx.liveCalls)
	}
	if result.Source != conversation.SearchSourceSemantic {
		t.Fatalf("source = %v, want semantic", result.Source)
	}
	if result.ReturnedCount != 0 || result.Offset != 1 || result.NextOffset != 1 {
		t.Fatalf("result = %+v, want an empty semantic page at offset 1", result)
	}
}

func TestSearchConversationsResultEngineErrorFallsBackLiteral(t *testing.T) {
	t.Parallel()
	live := conversation.SearchConversationsResult{
		Matches: []conversation.SearchMatch{
			{
				Record:        daemonTestRecord("claude:lit", false),
				MessageIndex:  0,
				Role:          "user",
				Timestamp:     time.Unix(3, 0),
				Snippet:       "literal match",
				Score:         0,
				ContextWindow: "",
			},
		},
		ConversationsScanned: 1,
		ReturnedCount:        1,
		Limit:                10,
		HasMore:              false,
		Source:               conversation.SearchSourceLiteral,
		Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
		Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting:     nil,
	}
	idx := &fakeSearchIndex{records: nil, live: live, liveErr: nil, liveCalls: 0}
	semantic := &fakeSemanticSearch{hits: nil, err: errors.New("engine down"), filters: nil}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", true, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 1 {
		t.Fatalf("live scan called %d times, want 1 on engine error", idx.liveCalls)
	}
	if result.Source != conversation.SearchSourceLiteral {
		t.Fatalf("source = %v, want literal on engine-error fallback", result.Source)
	}
	if result.ReturnedCount != 1 || result.Matches[0].Record.ID != "claude:lit" {
		t.Fatalf("fallback result = %+v, want the live literal match", result)
	}
}

func TestSearchConversationsResultEngineEmptyFallsBackLiteral(t *testing.T) {
	t.Parallel()
	live := conversation.SearchConversationsResult{
		Matches: []conversation.SearchMatch{
			{
				Record:        daemonTestRecord("claude:lit", false),
				MessageIndex:  0,
				Role:          "user",
				Timestamp:     time.Unix(3, 0),
				Snippet:       "literal match",
				Score:         0,
				ContextWindow: "",
			},
		},
		ConversationsScanned: 1,
		ReturnedCount:        1,
		Limit:                10,
		HasMore:              false,
		Source:               conversation.SearchSourceLiteral,
		Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
		Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting:     nil,
	}
	idx := &fakeSearchIndex{records: nil, live: live, liveErr: nil, liveCalls: 0}
	semantic := &fakeSemanticSearch{hits: nil, err: nil, filters: nil}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", true, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 1 {
		t.Fatalf("live scan called %d times, want 1 on empty engine result", idx.liveCalls)
	}
	if result.Source != conversation.SearchSourceLiteral {
		t.Fatalf("source = %v, want literal on empty-engine fallback", result.Source)
	}
}

// TestSearchConversationsResultLiteralDisabledCold proves the dogfood switch:
// with the engine configured but empty and literalFallback off, the result is a
// loud LiteralDisabledCold with no matches and no literal scan.
func TestSearchConversationsResultLiteralDisabledCold(t *testing.T) {
	t.Parallel()
	idx := &fakeSearchIndex{
		records:   map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	semantic := &fakeSemanticSearch{hits: nil, err: nil, filters: nil}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 0 {
		t.Fatalf("live scan called %d times, want 0 when literal fallback is disabled", idx.liveCalls)
	}
	if result.Source != conversation.SearchSourceLiteralDisabledCold {
		t.Fatalf("source = %v, want literal_disabled_cold", result.Source)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("matches = %d, want 0 on a cold disabled result", len(result.Matches))
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
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	semantic := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 0, Role: "user", TimestampUnix: 5, Content: "hit", Score: 0.5, ParentConversationID: ""},
		},
		err:     nil,
		filters: nil,
	}
	providerScoped := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10, Provider: clydev1.Provider_PROVIDER_CLAUDE}

	if _, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, providerScoped); err != nil {
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
	if _, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, workspaceScoped); err != nil {
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
	if _, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", false, unscoped); err != nil {
		t.Fatalf("unscoped search: %v", err)
	}
	if len(semantic.filters) != 3 {
		t.Fatalf("engine calls = %d, want 3", len(semantic.filters))
	}
	if len(semantic.filters[2].ConversationIDs) != 0 || len(semantic.filters[2].Providers) != 0 {
		t.Fatalf("unscoped filter = %+v, want no provider or id scope", semantic.filters[2])
	}
}

func TestSearchConversationsResultNoEngineLiveOnly(t *testing.T) {
	t.Parallel()
	live := conversation.SearchConversationsResult{
		Matches:              nil,
		ConversationsScanned: 0,
		ReturnedCount:        0,
		Limit:                10,
		HasMore:              false,
		Source:               conversation.SearchSourceLiteral,
		Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
		Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting:     nil,
	}
	idx := &fakeSearchIndex{records: nil, live: live, liveErr: nil, liveCalls: 0}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 0, Offset: -5}

	result, err := searchConversationsResult(context.Background(), idx, nil, "", false, req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 1 {
		t.Fatalf("live scan called %d times, want 1 when no engine is configured", idx.liveCalls)
	}
	if idx.lastLive.Limit != conversation.DefaultSearchLimit || idx.lastLive.Offset != 0 {
		t.Fatalf("live options = %+v, want normalized default limit and zero offset", idx.lastLive)
	}
	if result.Source != conversation.SearchSourceLiteral {
		t.Fatalf("source = %v, want literal when no engine is configured", result.Source)
	}
}
