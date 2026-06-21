package daemon

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
)

func testBackfillRecord(id, workspace string, archived bool) conversation.Record {
	return conversation.Record{
		ID:            id,
		Provider:      conversation.ProviderClaude,
		NativeID:      id,
		Lineage:       nil,
		Title:         "",
		WorkspaceRoot: workspace,
		ArtifactPath:  "/tmp/clyde-backfill-test.jsonl",
		ArtifactKind:  "jsonl",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      archived,
	}
}

func TestBuildBackfillEntriesProjectsIDWorkspaceAndArchived(t *testing.T) {
	t.Parallel()
	records := []conversation.Record{
		testBackfillRecord("claude:one", "/repo/alpha", false),
		testBackfillRecord("codex:two", "/repo/beta", true),
	}

	entries := buildBackfillEntries(records)

	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].ConversationID != "claude:one" || entries[0].WorkspaceRoot != "/repo/alpha" || entries[0].Archived {
		t.Fatalf("entries[0] = %+v, want {claude:one /repo/alpha false}", entries[0])
	}
	if entries[1].ConversationID != "codex:two" || entries[1].WorkspaceRoot != "/repo/beta" || !entries[1].Archived {
		t.Fatalf("entries[1] = %+v, want {codex:two /repo/beta true}", entries[1])
	}
}

func TestBuildBackfillEntriesEmpty(t *testing.T) {
	t.Parallel()
	entries := buildBackfillEntries(nil)
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
}
