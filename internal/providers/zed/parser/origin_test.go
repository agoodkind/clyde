package parser

import (
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
)

// zedThreadRecord writes one Zed thread row plus its sidebar metadata, then
// discovers and scans it through the parser's public boundary.
func zedThreadRecord(t *testing.T, threadID string, agentID string, threadJSON string) (conversation.Record, bool) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 12, 0, 0, 0, time.UTC)

	writeThreadsRow(t, root, threadID, "", updatedAt, []byte(threadJSON))
	writeSidebarRowWithOptions(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), sidebarRowOptions{
		SessionID:              threadID,
		AgentID:                agentID,
		Title:                  "Thread title",
		TitleOverride:          "",
		UpdatedAt:              updatedAt,
		CreatedAt:              updatedAt,
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		Archived:               false,
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
	})

	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want the thread discovered so its origin can be classified", len(candidates))
	}
	return parser.ScanRecord(candidates[0].Path, candidates[0].Stamp)
}

const plainZedThreadJSON = `{"version":"0.3.0","title":"Thread title","updated_at":"2026-06-27T12:00:00Z","model":{"provider":"anthropic","model":"claude-sonnet"},"messages":[]}`

// TestDiscoverAdmitsAgentThreadsSoTheSettingGovernsThem proves the always-on
// agent-id skip in Discover folded into the conversation setting: an agent thread
// is discovered and classified as subagent origin rather than dropped, so the one
// setting decides whether it is served.
func TestDiscoverAdmitsAgentThreadsSoTheSettingGovernsThem(t *testing.T) {
	record, ok := zedThreadRecord(t, "agent-thread", "claude", plainZedThreadJSON)
	if !ok {
		t.Fatal("ScanRecord returned ok=false for an agent thread")
	}
	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q from the sidebar agent id", record.Origin, conversation.OriginSubagent)
	}
	if !record.IsSubagent() {
		t.Fatalf("IsSubagent = false, want true for an agent-owned thread")
	}
	// Zed thread ids are the thread's own, so unlike Claude there is no parent
	// collision to avoid.
	if record.NativeID != "agent-thread" {
		t.Fatalf("native id = %q, want the thread's own id", record.NativeID)
	}
}

// TestScanRecordClassifiesSubagentContextAsSubagentOrigin covers Zed's second
// marker: a thread document that persists a parent thread context.
func TestScanRecordClassifiesSubagentContextAsSubagentOrigin(t *testing.T) {
	threadJSON := `{"version":"0.3.0","title":"Thread title","updated_at":"2026-06-27T12:00:00Z","model":{"provider":"anthropic","model":"claude-sonnet"},"subagent_context":{"parent_thread_id":"parent-thread","depth":1},"messages":[]}`

	record, ok := zedThreadRecord(t, "spawned-thread", "", threadJSON)
	if !ok {
		t.Fatal("ScanRecord returned ok=false")
	}
	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q from the persisted subagent context", record.Origin, conversation.OriginSubagent)
	}
	if record.Lineage == nil || record.Lineage.ParentNativeID != "parent-thread" {
		t.Fatalf("lineage = %#v, want the parent thread carried", record.Lineage)
	}
}

// TestScanRecordClassifiesAnUnmarkedZedThreadAsUserOrigin is the other side: a
// thread with neither marker belongs to the person.
func TestScanRecordClassifiesAnUnmarkedZedThreadAsUserOrigin(t *testing.T) {
	record, ok := zedThreadRecord(t, "user-thread", "", plainZedThreadJSON)
	if !ok {
		t.Fatal("ScanRecord returned ok=false")
	}
	if record.Origin != conversation.OriginUser {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginUser)
	}
	if record.IsSubagent() {
		t.Fatalf("IsSubagent = true, want false for an unmarked thread")
	}
}
