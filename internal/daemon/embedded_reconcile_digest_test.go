package daemon

import (
	"testing"
)

// TestReconciliationKeepsRecordedDigests acknowledges one batch with a chat
// field and rebuilds the owner from a published field list with that field
// and one field key that the outbox never recorded. The recorded field must
// keep its digest. The new field key must record embeddedUnknownFieldDigest.
func TestReconciliationKeepsRecordedDigests(t *testing.T) {
	store, _ := openBlockedTestStore(t)
	const ownerID = "codex:reconcile-digest"
	generation := recordOwnerTestBatch(t, store, ownerID, "recorded field text")
	if err := store.outbox.acknowledgeBatch(t.Context(), generation.batch, "receipt-fingerprint"); err != nil {
		t.Fatalf("acknowledge batch: %v", err)
	}
	known := generation.rows[0]
	const unknownFieldKey = "p1|v1;/m7/chat"
	fields := []embeddedReconciledField{
		{FieldKey: known.FieldKey, GenerationOrder: generation.batch.GenerationOrder},
		{FieldKey: unknownFieldKey, GenerationOrder: generation.batch.GenerationOrder},
	}
	if err := store.outbox.rebuildOwner(t.Context(), store.namespace.ID, ownerID, fields, generation.batch.Metadata, 0, nil); err != nil {
		t.Fatalf("rebuild owner: %v", err)
	}

	committed, err := store.outbox.committedFields(t.Context(), store.namespace.ID, ownerID)
	if err != nil {
		t.Fatalf("read committed fields: %v", err)
	}
	if committed[known.FieldKey] != known.FieldDigest || committed[unknownFieldKey] != embeddedUnknownFieldDigest || len(committed) != 2 {
		t.Fatalf("committed digests = %v, want %s=%s and %s=%s", committed,
			known.FieldKey, known.FieldDigest, unknownFieldKey, embeddedUnknownFieldDigest)
	}
}
