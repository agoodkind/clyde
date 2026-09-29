package daemon

import (
	"log/slog"
	"testing"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

const (
	ownerReconciledMessage     = "daemon.conversation_semantic_embedded.owner_reconciled"
	generationAbortedMessage   = "daemon.conversation_semantic_embedded.generation_aborted"
	reconcileKnownOwnerID      = "codex:" + embeddedProjectionCodexThreadID
	reconcileUnknownOwnerID    = "codex:" + embeddedSubagentThreadID
	reconcileExternalOrderTwo  = 2
	reconcileExternalTokenTwo  = "external-generation-2"
	reconcileKnownOwnerToken   = "known-owner-generation-1"
	reconcileUnknownOwnerToken = "unknown-owner-generation-1"
)

// TestEmbeddedReconciliationRebuildsPartiallyLostOutbox commits generation 1
// for two conversations through the library. The outbox records and
// acknowledges the generation of the first conversation and has no state for
// the second. One embedded pass must reconcile only the second conversation,
// apply its current metadata through ReprojectScalars at projection order 1,
// and block no owner.
func TestEmbeddedReconciliationRebuildsPartiallyLostOutbox(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	ageLiveArtifactForGate(t, writeEmbeddedProjectionCodexRollout(t, stores))
	writeEmbeddedSubagentRollout(t, stores)
	index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh index: %v", err)
	}
	store, capture := openBlockedTestStore(t)
	records := reconcileTestRecords(t, index)
	acknowledgeReconcileTestGeneration(t, store, records[reconcileKnownOwnerID], reconcileKnownOwnerToken)
	commitReconcileTestGeneration(t, store, reconcileUnknownOwnerID, 1, reconcileUnknownOwnerToken)

	worker := newReconcileTestWorker(store, index)
	if err := worker.runPass(t.Context()); err != nil {
		t.Fatalf("run embedded pass: %v", err)
	}

	reconciledOwners := reconcileTestMessageOwners(capture, ownerReconciledMessage)
	if len(reconciledOwners) != 1 || reconciledOwners[0] != reconcileUnknownOwnerID {
		t.Fatalf("reconciled owners = %q, want only %s", reconciledOwners, reconcileUnknownOwnerID)
	}
	for ownerID, wantProjectionOrder := range map[string]uint64{reconcileUnknownOwnerID: 1, reconcileKnownOwnerID: 0} {
		listed, err := store.library.ListOwnerOccurrences(t.Context(), store.namespace.ID, ownerID)
		if err != nil || listed.ProjectionOrder != wantProjectionOrder {
			t.Fatalf("owner %s projection order = %d, %v, want %d", ownerID, listed.ProjectionOrder, err, wantProjectionOrder)
		}
	}
	assertNoBlockedReconcileOwners(t, store, capture)
}

// TestEmbeddedReconciliationClearsBlockedOwner records a pending batch at
// generation order 1, commits order 2 through the library directly, and lets
// the replay block the owner with library.ErrStaleGeneration. One embedded pass
// must abort the blocked token, rebuild the owner, clear the blocked state, and
// log one aborted token and one rebuilt owner.
func TestEmbeddedReconciliationClearsBlockedOwner(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	ageLiveArtifactForGate(t, writeEmbeddedProjectionCodexRollout(t, stores))
	index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh index: %v", err)
	}
	store, capture := openBlockedTestStore(t)
	records := reconcileTestRecords(t, index)
	generation := prepareReconcileTestGeneration(t, store, records[reconcileKnownOwnerID])
	if err := store.outbox.recordBatch(t.Context(), generation.batch, generation.rows); err != nil {
		t.Fatalf("record batch: %v", err)
	}
	commitReconcileTestGeneration(t, store, reconcileKnownOwnerID, reconcileExternalOrderTwo, reconcileExternalTokenTwo)
	if _, err := store.delivery.replayPending(t.Context()); err != nil {
		t.Fatalf("replay batches: %v", err)
	}
	blocked, err := store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil || len(blocked) != 1 {
		t.Fatalf("blocked owners before the pass = %q, %v, want the owner", blocked, err)
	}

	worker := newReconcileTestWorker(store, index)
	if err := worker.runPass(t.Context()); err != nil {
		t.Fatalf("run embedded pass: %v", err)
	}

	aborted := reconcileTestMessageOwners(capture, generationAbortedMessage)
	rebuilt := reconcileTestMessageOwners(capture, ownerReconciledMessage)
	if len(aborted) != 1 || len(rebuilt) != 1 {
		t.Fatalf("aborted token records/rebuilt owner records = %d/%d, want 1/1", len(aborted), len(rebuilt))
	}
	blocked, err = store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("blocked owners after the pass = %q, %v, want none", blocked, err)
	}
}

func newReconcileTestWorker(store *embeddedConversationStore, index *conversation.Index) *conversationSemanticSyncWorker {
	semantic := config.ConversationSemanticConfig{
		IngestionEnabled: true,
		CollectionID:     store.namespace.ID,
		Backend:          config.ConversationSemanticBackendEmbedded,
		IncludeSubagents: true,
	}
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.embedded = newEmbeddedConversationSync(semantic, "", newEmbeddedSemanticStatus(), index)
	worker.embedded.store = store
	return worker
}

func reconcileTestRecords(t *testing.T, index *conversation.Index) map[string]conversation.Record {
	t.Helper()
	stamped, err := index.ListAllWithStamps(t.Context())
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	records := make(map[string]conversation.Record, len(stamped))
	for _, stampedRecord := range stamped {
		records[stampedRecord.Record.ID] = stampedRecord.Record
	}
	return records
}

// prepareReconcileTestGeneration prepares a generation without rows for one
// record at the next library order.
func prepareReconcileTestGeneration(t *testing.T, store *embeddedConversationStore, record conversation.Record) embeddedGeneration {
	t.Helper()
	owner := newEmbeddedConversationOwner(record, defaultSemanticContentKinds())
	generation, err := store.delivery.prepareGeneration(t.Context(), embeddedOutboxBatch{
		BatchID:           "",
		Namespace:         store.namespace.ID,
		OwnerID:           record.ID,
		GenerationOrder:   0,
		SourcePath:        record.ArtifactPath,
		SourceStamp:       "1:1",
		ProjectionProfile: owner.ProjectionProfile,
		RowCount:          0,
		ManifestHash:      "",
		Metadata:          embeddedOwnerMetadataOf(record),
	}, nil)
	if err != nil {
		t.Fatalf("prepare generation of %s: %v", record.ID, err)
	}
	return generation
}

// acknowledgeReconcileTestGeneration commits a generation without rows for one
// record through the library and records and acknowledges it in the outbox.
func acknowledgeReconcileTestGeneration(t *testing.T, store *embeddedConversationStore, record conversation.Record, token string) {
	t.Helper()
	generation := prepareReconcileTestGeneration(t, store, record)
	generation.batch.BatchID = token
	receipt := commitReconcileTestGeneration(t, store, record.ID, generation.batch.GenerationOrder, token)
	if err := store.outbox.recordBatch(t.Context(), generation.batch, generation.rows); err != nil {
		t.Fatalf("record batch: %v", err)
	}
	if err := store.outbox.acknowledgeBatch(t.Context(), generation.batch, receipt.Fingerprint); err != nil {
		t.Fatalf("acknowledge batch: %v", err)
	}
}

func commitReconcileTestGeneration(t *testing.T, store *embeddedConversationStore, ownerID string, order uint64, token string) library.ApplyReceipt {
	t.Helper()
	receipt, err := store.library.Apply(t.Context(), library.Batch{
		Namespace:        store.namespace.ID,
		OwnerID:          ownerID,
		GenerationOrder:  order,
		IdempotencyToken: token,
		Mode:             library.Append,
		Rows:             nil,
	})
	if err != nil || receipt.GenerationOrder != order {
		t.Fatalf("commit generation %d of %s = %+v, %v", order, ownerID, receipt, err)
	}
	return receipt
}

// reconcileTestMessageOwners returns the conversation_id attribute of every
// captured record with message.
func reconcileTestMessageOwners(capture *semanticLogCapture, message string) []string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	var owners []string
	for _, record := range capture.records {
		if record.Message != message {
			continue
		}
		record.Attrs(func(attribute slog.Attr) bool {
			if attribute.Key == "conversation_id" {
				owners = append(owners, attribute.Value.String())
				return false
			}
			return true
		})
	}
	return owners
}

func assertNoBlockedReconcileOwners(t *testing.T, store *embeddedConversationStore, capture *semanticLogCapture) {
	t.Helper()
	blocked, err := store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("blocked owners = %q, %v, want none", blocked, err)
	}
	if owners := reconcileTestMessageOwners(capture, ownerBlockedMessage); len(owners) != 0 {
		t.Fatalf("owner_blocked records for %q, want none", owners)
	}
}

// TestEmbeddedReconcileCommandClearsBlockedOwner blocks one owner with a real
// library.ErrStaleGeneration and reconciles that conversation with the store.
// The owner must lose its blocked state with one aborted token. An unknown
// conversation ID must fail.
func TestEmbeddedReconcileCommandClearsBlockedOwner(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	ageLiveArtifactForGate(t, writeEmbeddedProjectionCodexRollout(t, stores))
	index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh index: %v", err)
	}
	store, _ := openBlockedTestStore(t)
	records := reconcileTestRecords(t, index)
	generation := prepareReconcileTestGeneration(t, store, records[reconcileKnownOwnerID])
	if err := store.outbox.recordBatch(t.Context(), generation.batch, generation.rows); err != nil {
		t.Fatalf("record batch: %v", err)
	}
	commitReconcileTestGeneration(t, store, reconcileKnownOwnerID, reconcileExternalOrderTwo, reconcileExternalTokenTwo)
	if _, err := store.delivery.replayPending(t.Context()); err != nil {
		t.Fatalf("replay batches: %v", err)
	}

	result, err := reconcileEmbeddedConversationWithStore(t.Context(), store, index, reconcileKnownOwnerID)
	if err != nil || result.abortedTokens != 1 {
		t.Fatalf("reconcile = %+v, %v, want one aborted token", result, err)
	}
	blocked, err := store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("blocked owners after reconcile = %q, %v, want none", blocked, err)
	}
	if _, err := reconcileEmbeddedConversationWithStore(t.Context(), store, index, "codex:not-indexed"); err == nil {
		t.Fatal("reconcile of an unindexed conversation succeeded")
	}
}
