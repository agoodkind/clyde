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

func TestBuildBackfillEntriesCoalescesDuplicateIDPreferringWorkspace(t *testing.T) {
	t.Parallel()
	// One derived conversation id can come from two artifacts (the same session
	// recorded under two project dirs): one record carries the real workspace, the
	// other is empty. The empty record must never overwrite the real one, in either
	// input order. Regression for CLYDE-538.
	cases := []struct {
		name    string
		records []conversation.Record
	}{
		{
			name: "empty before real",
			records: []conversation.Record{
				testBackfillRecord("claude:dup", "", false),
				testBackfillRecord("claude:dup", "/repo/real", false),
			},
		},
		{
			name: "real before empty",
			records: []conversation.Record{
				testBackfillRecord("claude:dup", "/repo/real", false),
				testBackfillRecord("claude:dup", "", false),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			entries := buildBackfillEntries(testCase.records)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1 coalesced entry", len(entries))
			}
			if entries[0].ConversationID != "claude:dup" || entries[0].WorkspaceRoot != "/repo/real" {
				t.Fatalf("entries[0] = %+v, want {claude:dup /repo/real false}", entries[0])
			}
		})
	}
}

func TestBuildBackfillEntriesDuplicateIDPrefersMostRecentWhenWorkspacesEqualPresence(t *testing.T) {
	t.Parallel()
	// When duplicate records are equal on workspace-root presence (here both carry
	// one), the more recently updated record wins, in either input order.
	older := testBackfillRecord("claude:dup", "/repo/old", false)
	older.UpdatedAt = time.Unix(100, 0)
	newer := testBackfillRecord("claude:dup", "/repo/new", true)
	newer.UpdatedAt = time.Unix(200, 0)

	for _, records := range [][]conversation.Record{
		{older, newer},
		{newer, older},
	} {
		entries := buildBackfillEntries(records)
		if len(entries) != 1 {
			t.Fatalf("entries = %d, want 1 coalesced entry", len(entries))
		}
		if entries[0].WorkspaceRoot != "/repo/new" || !entries[0].Archived {
			t.Fatalf("entries[0] = %+v, want newer {/repo/new true}", entries[0])
		}
	}
}
