package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
)

// trailingTestQuietAge is an artifact age past the one-interval pass deferral
// and inside the trailing settle window.
const trailingTestQuietAge = 5 * time.Minute

// TestEmbeddedPassWithholdsGrowingCursorTurn writes a Cursor agent transcript
// with one user turn, which the provider can still extend, and runs embedded
// sync passes. The artifact is older than the pass deferral and younger than
// embeddedTrailingSettleWindow. The first pass must record no batch. A later
// assistant line releases the user turn in the next pass. In a second
// transcript, moving the artifact modification time past the settle window
// with os.Chtimes releases the single turn. Each release records one batch
// with the chat field of message 0 only. The embedding endpoint is a closed
// local port, and each recorded batch stays pending.
func TestEmbeddedPassWithholdsGrowingCursorTurn(t *testing.T) {
	for _, release := range []string{"later_message", "settle_window"} {
		t.Run(release, func(t *testing.T) {
			stores := isolateEmbeddedProjectionStores(t)
			transcriptPath := filepath.Join(stores.cursorProjects, embeddedProjectionCursorProjectKey, "agent-transcripts",
				embeddedProjectionCursorConversation, embeddedProjectionCursorConversation+".jsonl")
			if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
				t.Fatal(err)
			}
			appendEmbeddedProjectionLines(t, transcriptPath, []string{
				`{"role":"user","message":{"content":[{"type":"text","text":"open the config"}]}}`,
			})
			setTrailingTestAge(t, transcriptPath, trailingTestQuietAge)
			index := newEmbeddedProjectionIndex()
			store, _ := openBlockedTestStore(t)
			worker := newReconcileTestWorker(t, store, index)
			runTrailingTestPass(t, worker, index)
			if pending := trailingTestPendingFieldKeys(t, store); len(pending) != 0 {
				t.Fatalf("pass with an open trailing turn recorded fields %q, want none", pending)
			}

			if release == "later_message" {
				appendEmbeddedProjectionLines(t, transcriptPath, []string{
					`{"role":"assistant","message":{"content":[{"type":"text","text":"part one"}]}}`,
				})
				setTrailingTestAge(t, transcriptPath, trailingTestQuietAge)
			} else {
				setTrailingTestAge(t, transcriptPath, embeddedTrailingSettleWindow+time.Minute)
			}
			runTrailingTestPass(t, worker, index)
			pending := trailingTestPendingFieldKeys(t, store)
			if len(pending) != 1 || !strings.HasSuffix(pending[0], "/m0/chat") {
				t.Fatalf("pass after the %s release recorded fields %q, want only the message 0 chat field", release, pending)
			}
		})
	}
}

func setTrailingTestAge(t *testing.T, path string, age time.Duration) {
	t.Helper()
	modified := time.Now().Add(-age)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("set %s age: %v", path, err)
	}
}

// runTrailingTestPass refreshes the index and runs one embedded pass.
func runTrailingTestPass(t *testing.T, worker *conversationSemanticSyncWorker, index *conversation.Index) {
	t.Helper()
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh index: %v", err)
	}
	if err := worker.runPass(t.Context()); err != nil {
		t.Fatalf("run embedded pass: %v", err)
	}
}

// trailingTestPendingFieldKeys returns the field keys of every row of every
// pending batch.
func trailingTestPendingFieldKeys(t *testing.T, store *embeddedConversationStore) []string {
	t.Helper()
	pending, err := store.outbox.pendingBatches(t.Context())
	if err != nil {
		t.Fatalf("read pending batches: %v", err)
	}
	var keys []string
	for _, batch := range pending {
		rows, err := store.outbox.batchRows(t.Context(), batch.BatchID)
		if err != nil {
			t.Fatalf("read batch rows: %v", err)
		}
		for _, row := range rows {
			keys = append(keys, row.FieldKey)
		}
	}
	return keys
}
