package daemon

import (
	"context"
	"errors"
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
}

func (f *fakeSearchIndex) RecordByID(id string) (conversation.Record, bool) {
	record, ok := f.records[id]
	return record, ok
}

func (f *fakeSearchIndex) ConversationIDsMatching(_ context.Context, provider conversation.Provider, workspaceRoot string, includeArchived bool) ([]string, error) {
	conversationIDs := make([]string, 0, len(f.records))
	for id, record := range f.records {
		if conversation.RecordMatchesFilter(record, provider, workspaceRoot, includeArchived) {
			conversationIDs = append(conversationIDs, id)
		}
	}
	return conversationIDs, nil
}

func (f *fakeSearchIndex) SearchConversations(context.Context, conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error) {
	f.liveCalls++
	return f.live, f.liveErr
}

// fakeSemanticSearch is a canned engine-backed search client. It records the
// filter each across-search received so tests can prove id-set pushdown.
type fakeSemanticSearch struct {
	hits    []semsearch.SemHit
	err     error
	filters []semsearch.SearchFilter
}

func (f *fakeSemanticSearch) SearchConversations(_ context.Context, _ string, _ string, _ int32, filter semsearch.SearchFilter, _ int32) ([]semsearch.SemHit, error) {
	f.filters = append(f.filters, filter)
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

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 0 {
		t.Fatalf("live scan called %d times, want 0 on engine hit", idx.liveCalls)
	}
	if result.Warming {
		t.Fatalf("warming = true, want false when the engine produced a match")
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

func TestSearchConversationsResultEngineErrorFallsBackWarming(t *testing.T) {
	t.Parallel()
	live := conversation.SearchConversationsResult{
		Matches: []conversation.SearchMatch{
			{
				Record:       daemonTestRecord("claude:lit", false),
				MessageIndex: 0,
				Role:         "user",
				Timestamp:    time.Unix(3, 0),
				Snippet:      "literal match",
			},
		},
		ConversationsScanned: 1,
		ReturnedCount:        1,
		Limit:                10,
		HasMore:              false,
		Warming:              false,
	}
	idx := &fakeSearchIndex{records: nil, live: live, liveErr: nil, liveCalls: 0}
	semantic := &fakeSemanticSearch{hits: nil, err: errors.New("engine down")}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 1 {
		t.Fatalf("live scan called %d times, want 1 on engine error", idx.liveCalls)
	}
	if !result.Warming {
		t.Fatalf("warming = false, want true on engine-error fallback")
	}
	if result.ReturnedCount != 1 || result.Matches[0].Record.ID != "claude:lit" {
		t.Fatalf("fallback result = %+v, want the live literal match", result)
	}
}

func TestSearchConversationsResultEngineEmptyFallsBackWarming(t *testing.T) {
	t.Parallel()
	live := conversation.SearchConversationsResult{
		Matches: []conversation.SearchMatch{
			{
				Record:       daemonTestRecord("claude:lit", false),
				MessageIndex: 0,
				Role:         "user",
				Timestamp:    time.Unix(3, 0),
				Snippet:      "literal match",
			},
		},
		ConversationsScanned: 1,
		ReturnedCount:        1,
		Limit:                10,
		HasMore:              false,
		Warming:              false,
	}
	idx := &fakeSearchIndex{records: nil, live: live, liveErr: nil, liveCalls: 0}
	semantic := &fakeSemanticSearch{hits: nil, err: nil}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 1 {
		t.Fatalf("live scan called %d times, want 1 on empty engine result", idx.liveCalls)
	}
	if !result.Warming {
		t.Fatalf("warming = false, want true on empty-engine fallback")
	}
}

// TestSearchConversationsResultPushesIDScope proves a provider-scoped request
// resolves matching records into the conversation-id set the engine receives,
// and an unscoped request pushes no id set.
func TestSearchConversationsResultPushesIDScope(t *testing.T) {
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
	scoped := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10, Provider: clydev1.Provider_PROVIDER_CLAUDE}

	if _, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", scoped); err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if len(semantic.filters) != 1 {
		t.Fatalf("engine calls = %d, want 1", len(semantic.filters))
	}
	scopedFilter := semantic.filters[0]
	if len(scopedFilter.ConversationIDs) != 1 || scopedFilter.ConversationIDs[0] != "claude:one" {
		t.Fatalf("scoped filter ids = %v, want [claude:one]", scopedFilter.ConversationIDs)
	}

	unscoped := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}
	if _, err := searchConversationsResult(context.Background(), idx, semantic, "conversations", unscoped); err != nil {
		t.Fatalf("unscoped search: %v", err)
	}
	if len(semantic.filters) != 2 {
		t.Fatalf("engine calls = %d, want 2", len(semantic.filters))
	}
	if len(semantic.filters[1].ConversationIDs) != 0 {
		t.Fatalf("unscoped filter ids = %v, want none", semantic.filters[1].ConversationIDs)
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
		Warming:              false,
	}
	idx := &fakeSearchIndex{records: nil, live: live, liveErr: nil, liveCalls: 0}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	result, err := searchConversationsResult(context.Background(), idx, nil, "", req)
	if err != nil {
		t.Fatalf("search conversations result: %v", err)
	}
	if idx.liveCalls != 1 {
		t.Fatalf("live scan called %d times, want 1 when no engine is configured", idx.liveCalls)
	}
	if result.Warming {
		t.Fatalf("warming = true, want false when no engine is configured")
	}
}
