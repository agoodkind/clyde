package conversation

import (
	"context"
	"iter"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

func TestFilterRecordsDefaultsToBoundedPage(t *testing.T) {
	t.Parallel()
	records := make([]Record, 0, 25)
	for i := 0; i < 25; i++ {
		records = append(records, testListRecord("claude:"+strconv.Itoa(i), ProviderClaude))
	}

	result := FilterRecords(records, ListOptions{})

	if result.ReturnedCount != DefaultListLimit {
		t.Fatalf("returned count = %d, want %d", result.ReturnedCount, DefaultListLimit)
	}
	if result.TotalMatched != 25 {
		t.Fatalf("total matched = %d, want 25", result.TotalMatched)
	}
	if !result.HasMore {
		t.Fatalf("has more = false, want true")
	}
	if result.NextOffset != DefaultListLimit {
		t.Fatalf("next offset = %d, want %d", result.NextOffset, DefaultListLimit)
	}
	if result.Records[0].ID != "claude:0" || result.Records[19].ID != "claude:19" {
		t.Fatalf("page ids = %q..%q, want claude:0..claude:19", result.Records[0].ID, result.Records[19].ID)
	}
}

func TestFilterRecordsAppliesFiltersBeforePaging(t *testing.T) {
	t.Parallel()
	records := []Record{
		testListRecord("claude:auth", ProviderClaude),
		testListRecord("codex:session", ProviderCodex),
		testListRecord("codex:archived", ProviderCodex),
	}
	records[0].Title = "Auth fix"
	records[0].WorkspaceRoot = "/repo/a"
	records[1].Title = "Session"
	records[1].WorkspaceRoot = "/repo/b"
	records[1].ArtifactPath = "/tmp/session.jsonl"
	records[1].Model = "gpt-5"
	records[2].Title = "Archived auth"
	records[2].WorkspaceRoot = "/repo/b"
	records[2].Archived = true

	result := FilterRecords(records, ListOptions{
		Limit:         10,
		Provider:      ProviderCodex,
		WorkspaceRoot: "/repo/b",
		Query:         "session gpt-5",
	})

	if result.TotalMatched != 1 || result.ReturnedCount != 1 {
		t.Fatalf("matched=%d returned=%d, want 1 and 1", result.TotalMatched, result.ReturnedCount)
	}
	if result.Records[0].ID != "codex:session" {
		t.Fatalf("record id = %q, want codex:session", result.Records[0].ID)
	}

	archived := FilterRecords(records, ListOptions{
		Limit:           10,
		Provider:        ProviderCodex,
		WorkspaceRoot:   "/repo/b",
		Query:           "archived auth",
		IncludeArchived: true,
	})
	if archived.TotalMatched != 1 || archived.Records[0].ID != "codex:archived" {
		t.Fatalf("archived result = %+v, want codex:archived", archived)
	}
}

func TestSearchConversationsReturnsBoundedTranscriptMatches(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	parser := &messageMapParser{
		provider: providerid.ProviderClaude,
		messages: map[string][]transcript.Message{
			"/tmp/one.jsonl": {
				{Role: "user", Text: "nothing relevant"},
			},
			"/tmp/two.jsonl": {
				{Role: "assistant", Text: "The auth timeout happens during login.", Timestamp: time.Unix(20, 0)},
			},
			"/tmp/three.jsonl": {
				{Role: "user", Text: "Another auth timeout note.", Timestamp: time.Unix(30, 0)},
			},
		},
	}
	registry.Register(parser)
	idx := &Index{
		mu:           sync.Mutex{},
		registry:     registry,
		records:      []Record{testSearchRecord("claude:one", "/tmp/one.jsonl"), testSearchRecord("claude:two", "/tmp/two.jsonl"), testSearchRecord("claude:three", "/tmp/three.jsonl")},
		prevRecords:  nil,
		prevStamps:   nil,
		loaded:       true,
		refreshing:   false,
		lastRefresh:  time.Now(),
		cachePath:    "",
		debounce:     time.Hour,
		scanProvider: scan,
	}

	result, err := idx.SearchConversations(context.Background(), SearchConversationsOptions{
		Query: "auth timeout",
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("search conversations: %v", err)
	}
	if result.ReturnedCount != 1 {
		t.Fatalf("returned count = %d, want 1", result.ReturnedCount)
	}
	if result.ConversationsScanned != 2 {
		t.Fatalf("scanned = %d, want 2", result.ConversationsScanned)
	}
	if !result.HasMore {
		t.Fatalf("has more = false, want true")
	}
	match := result.Matches[0]
	if match.Record.ID != "claude:two" {
		t.Fatalf("match record = %q, want claude:two", match.Record.ID)
	}
	if match.MessageIndex != 0 || match.Role != "assistant" {
		t.Fatalf("match position = %d/%s, want 0/assistant", match.MessageIndex, match.Role)
	}
	if match.Snippet != "The auth timeout happens during login." {
		t.Fatalf("snippet = %q", match.Snippet)
	}
}

func TestSearchConversationsAppliesOffsetToTranscriptMatches(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	parser := &messageMapParser{
		provider: providerid.ProviderClaude,
		messages: map[string][]transcript.Message{
			"/tmp/one.jsonl": {
				{Role: "user", Text: "nothing relevant"},
			},
			"/tmp/two.jsonl": {
				{Role: "assistant", Text: "The auth timeout happens during login.", Timestamp: time.Unix(20, 0)},
			},
			"/tmp/three.jsonl": {
				{Role: "user", Text: "Another auth timeout note.", Timestamp: time.Unix(30, 0)},
			},
		},
	}
	registry.Register(parser)
	idx := &Index{
		mu:           sync.Mutex{},
		registry:     registry,
		records:      []Record{testSearchRecord("claude:one", "/tmp/one.jsonl"), testSearchRecord("claude:two", "/tmp/two.jsonl"), testSearchRecord("claude:three", "/tmp/three.jsonl")},
		prevRecords:  nil,
		prevStamps:   nil,
		loaded:       true,
		refreshing:   false,
		lastRefresh:  time.Now(),
		cachePath:    "",
		debounce:     time.Hour,
		scanProvider: scan,
	}

	result, err := idx.SearchConversations(context.Background(), SearchConversationsOptions{
		Query:  "auth timeout",
		Limit:  1,
		Offset: 1,
	})
	if err != nil {
		t.Fatalf("search conversations: %v", err)
	}
	if result.Offset != 1 || result.NextOffset != 2 {
		t.Fatalf("offsets = %d/%d, want 1/2", result.Offset, result.NextOffset)
	}
	if result.ReturnedCount != 1 {
		t.Fatalf("returned count = %d, want 1", result.ReturnedCount)
	}
	if result.HasMore {
		t.Fatalf("has more = true, want false")
	}
	if result.Matches[0].Record.ID != "claude:three" {
		t.Fatalf("match record = %q, want claude:three", result.Matches[0].Record.ID)
	}
}

func TestSearchConversationsMatchesRenderedMessageIndexText(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	parser := &messageMapParser{
		provider: providerid.ProviderClaude,
		messages: map[string][]transcript.Message{
			"/tmp/tools.jsonl": {
				{
					Role:      "assistant",
					Text:      "ordinary prose",
					Thinking:  "reasoning-needle",
					Timestamp: time.Unix(40, 0),
					HasTools:  true,
					Tools: []transcript.ToolCall{
						{
							Name:   "Bash",
							Input:  transcript.ToolInputJSON{Raw: []byte(`{"command":"printf command-needle"}`)},
							Output: "output-needle",
						},
					},
				},
			},
		},
		loadOptions: nil,
	}
	registry.Register(parser)
	idx := &Index{
		mu:           sync.Mutex{},
		registry:     registry,
		records:      []Record{testSearchRecord("claude:tools", "/tmp/tools.jsonl")},
		prevRecords:  nil,
		prevStamps:   nil,
		loaded:       true,
		refreshing:   false,
		lastRefresh:  time.Now(),
		cachePath:    "",
		debounce:     time.Hour,
		scanProvider: scan,
	}

	result, err := idx.SearchConversations(context.Background(), SearchConversationsOptions{
		Query: "reasoning-needle command-needle output-needle",
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("search conversations: %v", err)
	}

	if result.ReturnedCount != 1 {
		t.Fatalf("returned count = %d, want 1", result.ReturnedCount)
	}
	if len(parser.loadOptions) != 1 {
		t.Fatalf("load options calls = %d, want 1", len(parser.loadOptions))
	}
	if !parser.loadOptions[0].IncludeToolOutputs {
		t.Fatalf("IncludeToolOutputs = false, want true")
	}
	snippet := result.Matches[0].Snippet
	for _, want := range []string{"reasoning-needle", "command-needle", "output-needle"} {
		if !strings.Contains(snippet, want) {
			t.Fatalf("snippet = %q, missing %q", snippet, want)
		}
	}
}

type messageMapParser struct {
	provider    providerid.Provider
	messages    map[string][]transcript.Message
	loadOptions []LoadOptions
}

func (p *messageMapParser) Provider() providerid.Provider { return p.provider }

func (*messageMapParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (*messageMapParser) ScanRecord(string, FileStamp) (Record, bool) {
	return emptyRecord(), false
}

func (p *messageMapParser) Stream(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
	p.loadOptions = append(p.loadOptions, opts)
	return func(yield func(transcript.Message, error) bool) {
		for _, message := range p.messages[path] {
			if !yield(message, nil) {
				return
			}
		}
	}
}

func testListRecord(id string, provider Provider) Record {
	return Record{
		ID:            id,
		Provider:      provider,
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
		Archived:      false,
	}
}

func testSearchRecord(id string, path string) Record {
	record := testListRecord(id, ProviderClaude)
	record.ArtifactPath = path
	return record
}
