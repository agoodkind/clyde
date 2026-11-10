package parser

import (
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

// TestScanRecordClassifiesSpawnedCodexSubagentFromSessionMeta proves the origin
// comes from the rollout's session_meta record and that the parent thread id it
// names reaches the record as spawn lineage.
func TestScanRecordClassifiesSpawnedCodexSubagentFromSessionMeta(t *testing.T) {
	t.Parallel()
	threadID := "019de9bb-3a00-7010-bd9f-a6ee71559357"
	parentID := "019de9aa-spawn-parent"
	payload := `{"id":"` + threadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `","depth":1,"agent_path":"/agents/helper.md","agent_nickname":"helper","agent_role":"analysis"}}},"thread_source":"subagent","model_provider":"openai"}`

	record, ok := scanRecordFromCodexSessionMetaPayload(t, threadID, payload)
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginSubagent)
	}
	if !record.IsSubagent() {
		t.Fatalf("IsSubagent = false, want true for a spawned subagent thread")
	}
	if record.Lineage == nil || record.Lineage.ParentNativeID != parentID {
		t.Fatalf("lineage = %#v, want the spawning parent thread id %q", record.Lineage, parentID)
	}
	parentConversationID, ok := conversation.ParentConversationID(record)
	if !ok {
		t.Fatalf("ParentConversationID reported no parent for a spawned subagent")
	}
	wantParentConversationID := conversation.DerivedID(conversation.ProviderCodex, parentID, "")
	if parentConversationID != wantParentConversationID {
		t.Fatalf("parent conversation id = %q, want %q", parentConversationID, wantParentConversationID)
	}
}

// TestScanRecordClassifiesCodexUserThreadFromSessionMeta covers the other side of
// the same read: a plain string source with thread_source "user" is a person's
// own thread and names no parent.
func TestScanRecordClassifiesCodexUserThreadFromSessionMeta(t *testing.T) {
	t.Parallel()
	threadID := "019de9dd-3a00-7010-bd9f-a6ee71559357"
	payload := `{"id":"` + threadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"exec","thread_source":"user","model_provider":"openai"}`

	record, ok := scanRecordFromCodexSessionMetaPayload(t, threadID, payload)
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	if record.Origin != conversation.OriginUser {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginUser)
	}
	if record.IsSubagent() {
		t.Fatalf("IsSubagent = true, want false for a user thread")
	}
	if record.Lineage != nil {
		t.Fatalf("lineage = %#v, want nil for a user thread", record.Lineage)
	}
}
