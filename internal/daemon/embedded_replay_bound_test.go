package daemon

import (
	"testing"
)

const stageFailedMessage = "daemon.conversation_semantic_embedded.stage_failed"

// TestEmbeddedReplayStopsAtFirstTransientFailure records pending batches for
// two owners and replays them against a closed local embedding port. The first
// replay fails in Stage with a transient embedding error. The replay must not
// attempt the second batch in the same call, must keep both owners out of
// delivery, and must count the second batch as deferred.
func TestEmbeddedReplayStopsAtFirstTransientFailure(t *testing.T) {
	store, capture := openBlockedTestStore(t)
	recordOwnerTestBatch(t, store, "codex:replay-transient-a", "first pending owner text")
	recordOwnerTestBatch(t, store, "codex:replay-transient-b", "second pending owner text")

	result, err := store.delivery.replayPending(t.Context())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	attempted := reconcileTestMessageOwners(capture, stageFailedMessage)
	if len(attempted) != 1 || result.deferred != 1 || result.replayed != 0 {
		t.Fatalf("stage attempts/deferred/replayed = %q/%d/%d, want one attempt, 1 deferred, 0 replayed",
			attempted, result.deferred, result.replayed)
	}
	if !result.blockedOwners["codex:replay-transient-a"] || !result.blockedOwners["codex:replay-transient-b"] {
		t.Fatalf("owners kept out of delivery = %v, want both", result.blockedOwners)
	}
}

// TestEmbeddedReplayStopsAtByteBound records pending batches for three owners
// and commits a later generation for each owner through the library. Each
// replay then fails in Stage with the permanent library.ErrStaleGeneration
// before any embedding. The replay byte bound equals the stored row bytes of
// one batch. The replay must attempt only the first batch, block its owner,
// and leave the other two batches pending and deferred.
func TestEmbeddedReplayStopsAtByteBound(t *testing.T) {
	store, _ := openBlockedTestStore(t)
	owners := []string{"codex:replay-bytes-a", "codex:replay-bytes-b", "codex:replay-bytes-c"}
	var oneBatchBytes int64
	for _, ownerID := range owners {
		generation := recordOwnerTestBatch(t, store, ownerID, "pending owner text of equal length")
		oneBatchBytes = embeddedOutboxRowBytes(generation.rows)
		commitReconcileTestGeneration(t, store, ownerID, reconcileExternalOrderTwo, reconcileExternalTokenTwo)
	}
	store.delivery.maxReplayBytes = oneBatchBytes

	result, err := store.delivery.replayPending(t.Context())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	blocked, err := store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil {
		t.Fatalf("read blocked owners: %v", err)
	}
	pending, err := store.outbox.pendingBatches(t.Context())
	if err != nil {
		t.Fatalf("read pending batches: %v", err)
	}
	if len(blocked) != 1 || blocked[0] != owners[0] || len(pending) != 2 || result.deferred != 2 {
		t.Fatalf("blocked owners/pending batches/deferred = %q/%d/%d, want [%s]/2/2",
			blocked, len(pending), result.deferred, owners[0])
	}
}
