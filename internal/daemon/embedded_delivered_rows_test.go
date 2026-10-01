package daemon

import (
	"testing"
)

// TestAcknowledgedBatchKeepsNoStoredRows records one pending batch with a
// chat field and acknowledges it. The outbox must delete the stored rows of
// the batch and must keep the committed digest of its field.
func TestAcknowledgedBatchKeepsNoStoredRows(t *testing.T) {
	store, _ := openBlockedTestStore(t)
	generation := recordOwnerTestBatch(t, store, "codex:delivered-rows", "delivered owner text")
	if len(generation.rows) == 0 {
		t.Fatal("recorded batch has no rows")
	}
	if err := store.outbox.acknowledgeBatch(t.Context(), generation.batch, "receipt-fingerprint"); err != nil {
		t.Fatalf("acknowledge batch: %v", err)
	}

	outbox := openLiveReadOnlyForBlocked(t, conversationSemanticOutboxPath("blocked-test"))
	var storedRows int
	if err := outbox.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM batch_rows WHERE batch_id = ?`, generation.batch.BatchID).Scan(&storedRows); err != nil {
		t.Fatalf("count stored rows: %v", err)
	}
	committed, err := store.outbox.committedFields(t.Context(), store.namespace.ID, "codex:delivered-rows")
	if err != nil {
		t.Fatalf("read committed fields: %v", err)
	}
	field := generation.rows[0]
	if storedRows != 0 || committed[field.FieldKey] != field.FieldDigest {
		t.Fatalf("stored rows/committed digest = %d/%q, want 0/%q", storedRows, committed[field.FieldKey], field.FieldDigest)
	}
}
