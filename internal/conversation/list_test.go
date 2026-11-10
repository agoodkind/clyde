package conversation

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSnippetBoundsHugeInput(t *testing.T) {
	t.Parallel()
	// A matched message can now carry multi-megabyte tool output. snippet must
	// still return a bounded leading excerpt without normalizing the whole tail.
	head := strings.Repeat("token ", 400)
	huge := head + strings.Repeat("z", 8<<20)

	got := snippet(huge)

	if runes := []rune(got); len(runes) > searchSnippetRunes+3 {
		t.Fatalf("snippet rune count = %d, want <= %d", len(runes), searchSnippetRunes+3)
	}
	if !strings.HasPrefix(got, "token token") {
		t.Fatalf("snippet = %q, want leading tokens", got)
	}
}

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
