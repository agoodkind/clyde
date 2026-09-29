package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

const embeddedSubagentThreadID = "019de9bb-3a00-7010-bd9f-a6ee71559357"

// TestEmbeddedSubagentAdmissionIgnoresRawVisibility writes a spawned Codex
// subagent rollout and runs one embedded pass for each combination of
// conversation.include_subagent_conversations and
// conversation.semantic.include_subagents. The raw index listing must follow
// the raw setting. The embedded pass must record an outbox batch for the
// subagent conversation exactly when include_subagents is true, whatever the
// raw setting says. The embedding endpoint is a closed local port. The
// recorded batch therefore stays pending.
func TestEmbeddedSubagentAdmissionIgnoresRawVisibility(t *testing.T) {
	for _, testCase := range []struct {
		name             string
		rawVisible       bool
		embeddedIncludes bool
	}{
		{name: "raw_hidden_embedded_included", rawVisible: false, embeddedIncludes: true},
		{name: "raw_visible_embedded_excluded", rawVisible: true, embeddedIncludes: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stores := isolateEmbeddedProjectionStores(t)
			writeEmbeddedSubagentRollout(t, stores)
			index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{IncludeSubagentConversations: testCase.rawVisible})
			if err := index.Refresh(t.Context()); err != nil {
				t.Fatalf("refresh index: %v", err)
			}
			visible, err := index.ListWithStamps(t.Context())
			if err != nil {
				t.Fatalf("list visible records: %v", err)
			}
			if (len(visible) == 1) != testCase.rawVisible {
				t.Fatalf("raw listing returned %d records, want visible %t", len(visible), testCase.rawVisible)
			}

			store, _ := openBlockedTestStore(t)
			semantic := config.ConversationSemanticConfig{
				IngestionEnabled: true,
				CollectionID:     store.namespace.ID,
				Backend:          config.ConversationSemanticBackendEmbedded,
				IncludeSubagents: testCase.embeddedIncludes,
			}
			worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
			worker.embedded = newEmbeddedConversationSync(semantic, "", newEmbeddedSemanticStatus(), index)
			worker.embedded.store = store
			if err := worker.runPass(t.Context()); err != nil {
				t.Fatalf("run embedded pass: %v", err)
			}
			pending, err := store.outbox.pendingBatches(t.Context())
			if err != nil {
				t.Fatalf("read pending batches: %v", err)
			}
			recorded := len(pending) == 1 && pending[0].OwnerID == "codex:"+embeddedSubagentThreadID
			if recorded != testCase.embeddedIncludes || len(pending) > 1 {
				t.Fatalf("pending batches = %+v, want a subagent batch %t", pending, testCase.embeddedIncludes)
			}
		})
	}
}

// writeEmbeddedSubagentRollout writes a Codex rollout with a session_meta
// record that marks a spawned subagent thread, and moves the rollout
// modification time past the growing-artifact deferral.
func writeEmbeddedSubagentRollout(t *testing.T, stores embeddedProjectionStores) {
	t.Helper()
	dayDir := filepath.Join(stores.codexHome, "sessions", "2026", "05", "02")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, "rollout-2026-05-02T10-09-00-"+embeddedSubagentThreadID+".jsonl")
	appendEmbeddedProjectionLines(t, path, []string{
		`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"` + embeddedSubagentThreadID + `","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":{"subagent":{"thread_spawn":{"parent_thread_id":"019de9aa-spawn-parent","depth":1,"agent_path":"/agents/helper.md","agent_nickname":"helper","agent_role":"analysis"}}},"thread_source":"subagent","model_provider":"openai"}}`,
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"event_msg","payload":{"type":"user_message","message":"summarize the helper findings"}}`,
		`{"timestamp":"2026-05-02T17:09:09.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The helper found two issues."}]}}`,
	})
	ageLiveArtifactForGate(t, path)
}
