package daemon

import (
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

// TestReconciliationDeletesAbortedBatchRows records a pending batch with a
// chat field, commits a later generation for the owner through the library,
// and lets the replay block the batch with library.ErrStaleGeneration.
// Reconciliation of the owner must mark the batch aborted and delete its
// stored rows.
func TestReconciliationDeletesAbortedBatchRows(t *testing.T) {
	store, _ := openBlockedTestStore(t)
	const ownerID = "codex:aborted-rows"
	generation := recordOwnerTestBatch(t, store, ownerID, "aborted owner text")
	commitReconcileTestGeneration(t, store, ownerID, reconcileExternalOrderTwo, reconcileExternalTokenTwo)
	if _, err := store.delivery.replayPending(t.Context()); err != nil {
		t.Fatalf("replay batches: %v", err)
	}
	listed, err := store.library.ListOwnerOccurrences(t.Context(), store.namespace.ID, ownerID)
	if err != nil {
		t.Fatalf("list owner occurrences: %v", err)
	}
	record := conversation.Record{ID: ownerID, Provider: conversation.ProviderCodex}
	if _, err := store.delivery.reconcileOwner(t.Context(), store.namespace.ID, record, listed); err != nil {
		t.Fatalf("reconcile owner: %v", err)
	}

	outbox := openLiveReadOnlyForBlocked(t, conversationSemanticOutboxPath("blocked-test"))
	var state string
	var storedRows int
	if err := outbox.QueryRowContext(t.Context(), `SELECT state FROM batches WHERE batch_id = ?`, generation.batch.BatchID).Scan(&state); err != nil {
		t.Fatalf("read batch state: %v", err)
	}
	if err := outbox.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM batch_rows WHERE batch_id = ?`, generation.batch.BatchID).Scan(&storedRows); err != nil {
		t.Fatalf("count stored rows: %v", err)
	}
	if state != embeddedOutboxStateAborted || storedRows != 0 {
		t.Fatalf("batch state/stored rows = %s/%d, want %s/0", state, storedRows, embeddedOutboxStateAborted)
	}
}
