//go:build live

package daemon

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

const (
	liveEmbeddingBaseURL     = "http://localhost:5400/v1"
	liveEmbeddingModel       = "nvidia/NV-EmbedCode-7b-v1"
	liveEmbeddingDimension   = 4096
	liveEmbeddingAPIKeyEnv   = "OPENAI_API_KEY"
	liveMilvusAddress        = "localhost:19530"
	liveMilvusDatabasePrefix = "clyde_live_"
	liveCollectionID         = "clyde-conversations"
	liveOwnerID              = "codex:" + embeddedProjectionCodexThreadID
	// liveArtifactAge moves every artifact modification time past the
	// growing-artifact deferral.
	liveArtifactAge = 2 * time.Hour
)

// TestEmbeddedIngestionRecovery drives the embedded sync worker over a real
// Codex rollout artifact, a real SQLite catalog and outbox under temporary
// directories, a Milvus database that the test creates, and the real
// embedding endpoint. It covers first ingestion, an unchanged pass, a restart
// without field changes, a crash before CommitGeneration, a crash after
// CommitGeneration and before the outbox acknowledgment, appended messages,
// and source loss.
func TestEmbeddedIngestionRecovery(t *testing.T) {
	if strings.TrimSpace(os.Getenv(liveEmbeddingAPIKeyEnv)) == "" {
		t.Fatalf("environment variable %s is required for the embedding endpoint", liveEmbeddingAPIKeyEnv)
	}
	t.Logf("embedding window start %s", time.Now().UTC().Format(time.RFC3339))
	t.Cleanup(func() {
		t.Logf("embedding window end %s", time.Now().UTC().Format(time.RFC3339))
	})
	stores := isolateEmbeddedProjectionStores(t)
	database := createLiveMilvusDatabase(t)
	storeRoot := t.TempDir()
	rolloutPath := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, rolloutPath)
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	requireLiveOwner(t, index)

	uninterrupted := newLiveScenario(t, database, storeRoot, "uninterrupted", index)
	firstIngestion := uninterrupted.runPass(t)
	if firstIngestion.owner.GenerationOrder != 1 || firstIngestion.deliveredBatches != 1 || firstIngestion.pendingBatches != 0 {
		t.Fatalf("first ingestion order/delivered/pending = %d/%d/%d, want 1/1/0",
			firstIngestion.owner.GenerationOrder, firstIngestion.deliveredBatches, firstIngestion.pendingBatches)
	}
	if len(firstIngestion.committedFields) != 3 || len(firstIngestion.rows) != 3 {
		t.Fatalf("first ingestion committed fields/rows = %d/%d, want 3/3: %v",
			len(firstIngestion.committedFields), len(firstIngestion.rows), slices.Sorted(maps.Keys(firstIngestion.rows)))
	}
	if firstIngestion.vectors == 0 || firstIngestion.milvusVectors != int64(firstIngestion.vectors) || firstIngestion.vectorWriteGeneration == 0 {
		t.Fatalf("first ingestion catalog vectors/milvus vectors/write generation = %d/%d/%d, want equal nonzero vectors and a write",
			firstIngestion.vectors, firstIngestion.milvusVectors, firstIngestion.vectorWriteGeneration)
	}

	unchanged := uninterrupted.runPass(t)
	assertLiveSnapshotsEqual(t, "unchanged pass", firstIngestion, unchanged)
	uninterrupted.close(t)
	restarted := newLiveScenario(t, database, storeRoot, "uninterrupted", index)
	restartedPass := restarted.runPass(t)
	assertLiveSnapshotsEqual(t, "restarted unchanged pass", firstIngestion, restartedPass)
	restarted.close(t)

	crashBeforeCommit := newLiveScenario(t, database, storeRoot, "crash-before-commit", index)
	crashBeforeCommit.stageWithoutAcknowledgment(t, false)
	beforeReplay := crashBeforeCommit.snapshot(t)
	if beforeReplay.owner.GenerationOrder != 0 || beforeReplay.pendingBatches != 1 || len(beforeReplay.rows) != 0 {
		t.Fatalf("staged crash order/pending/rows = %d/%d/%d, want 0/1/0",
			beforeReplay.owner.GenerationOrder, beforeReplay.pendingBatches, len(beforeReplay.rows))
	}
	crashBeforeCommit.close(t)
	replayedBeforeCommit := newLiveScenario(t, database, storeRoot, "crash-before-commit", index)
	afterReplay := replayedBeforeCommit.runPass(t)
	assertLiveCommittedEqual(t, "crash before commit", firstIngestion, afterReplay)
	replayedBeforeCommit.close(t)

	crashAfterCommit := newLiveScenario(t, database, storeRoot, "crash-after-commit", index)
	savedReceipt := crashAfterCommit.stageWithoutAcknowledgment(t, true)
	committedBeforeAck := crashAfterCommit.snapshot(t)
	if committedBeforeAck.owner.GenerationOrder != 1 || committedBeforeAck.pendingBatches != 1 || committedBeforeAck.deliveredBatches != 0 {
		t.Fatalf("committed crash order/pending/delivered = %d/%d/%d, want 1/1/0",
			committedBeforeAck.owner.GenerationOrder, committedBeforeAck.pendingBatches, committedBeforeAck.deliveredBatches)
	}
	crashAfterCommit.close(t)
	replayedAfterCommit := newLiveScenario(t, database, storeRoot, "crash-after-commit", index)
	acknowledged := replayedAfterCommit.runPass(t)
	assertLiveCommittedEqual(t, "crash after commit", firstIngestion, acknowledged)
	if acknowledged.receipts["1"] != savedReceipt {
		t.Fatalf("acknowledged receipt = %q, want the saved receipt %q", acknowledged.receipts["1"], savedReceipt)
	}
	if acknowledged.vectorWriteGeneration != committedBeforeAck.vectorWriteGeneration || acknowledged.vectors != committedBeforeAck.vectors {
		t.Fatalf("replay after commit wrote vectors: write generation %d to %d, vectors %d to %d",
			committedBeforeAck.vectorWriteGeneration, acknowledged.vectorWriteGeneration, committedBeforeAck.vectors, acknowledged.vectors)
	}
	replayedAfterCommit.close(t)

	appendEmbeddedProjectionLines(t, rolloutPath, embeddedProjectionCodexAppendedLines)
	ageLiveArtifact(t, rolloutPath)
	refreshLiveIndex(t, index)
	appended := newLiveScenario(t, database, storeRoot, "uninterrupted", index)
	afterAppend := appended.runPass(t)
	if afterAppend.owner.GenerationOrder != 2 || afterAppend.deliveredBatches != 2 || len(afterAppend.committedFields) != 5 {
		t.Fatalf("append order/delivered/committed fields = %d/%d/%d, want 2/2/5",
			afterAppend.owner.GenerationOrder, afterAppend.deliveredBatches, len(afterAppend.committedFields))
	}
	assertLiveAppendedRows(t, firstIngestion.rows, afterAppend.rows)

	archivedPath := filepath.Join(stores.codexHome, "archived_sessions", filepath.Base(rolloutPath))
	if err := os.MkdirAll(filepath.Dir(archivedPath), 0o755); err != nil {
		t.Fatalf("create archived sessions directory: %v", err)
	}
	if err := os.Rename(rolloutPath, archivedPath); err != nil {
		t.Fatalf("archive rollout: %v", err)
	}
	refreshLiveIndex(t, index)
	afterArchive := appended.runPass(t)
	assertLiveSnapshotsEqual(t, "archive reprojection", afterAppend, afterArchive)
	archivedRows := countLiveEffectiveScalars(t, appended.semantic.CatalogPath, embeddedScalarArchived, true)
	if archivedRows != len(afterAppend.rows) || afterArchive.appliedProjections != 1 {
		t.Fatalf("archive reprojection archived rows/applied projections = %d/%d, want %d/1",
			archivedRows, afterArchive.appliedProjections, len(afterAppend.rows))
	}

	appended.close(t)
	removeLiveOutbox(t, appended.semantic.PoolID)
	if err := os.Rename(archivedPath, rolloutPath); err != nil {
		t.Fatalf("unarchive rollout: %v", err)
	}
	refreshLiveIndex(t, index)
	reconciled := newLiveScenario(t, database, storeRoot, "uninterrupted", index)
	afterOutboxLoss := reconciled.runPass(t)
	assertLiveOutboxLossReconciled(t, reconciled, afterArchive, afterOutboxLoss)

	if err := os.Remove(rolloutPath); err != nil {
		t.Fatalf("remove rollout: %v", err)
	}
	refreshLiveIndex(t, index)
	afterSourceLoss := reconciled.runPass(t)
	assertLiveSnapshotsEqual(t, "source loss", afterOutboxLoss, afterSourceLoss)
	if countLiveEffectiveScalars(t, reconciled.semantic.CatalogPath, embeddedScalarArchived, false) != len(afterAppend.rows) {
		t.Fatalf("source loss changed the archived metadata of committed rows")
	}
	reconciled.close(t)
}

// removeLiveOutbox deletes the outbox database of one pool with its WAL and
// shared-memory files.
func removeLiveOutbox(t *testing.T, poolID string) {
	t.Helper()
	path := conversationSemanticOutboxPath(poolID)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove outbox %s: %v", path+suffix, err)
		}
	}
}

// assertLiveOutboxLossReconciled requires the pass after outbox loss and an
// unarchive to keep every published row and vector, send no generation, record
// every published field under the unknown-digest marker, block no owner, and
// apply archived false to every row at projection order 2.
func assertLiveOutboxLossReconciled(t *testing.T, scenario *liveScenario, before liveSnapshot, after liveSnapshot) {
	t.Helper()
	if after.owner != before.owner || !maps.Equal(after.rows, before.rows) {
		t.Fatalf("outbox loss owner/rows = %+v/%d, want %+v/%d unchanged", after.owner, len(after.rows), before.owner, len(before.rows))
	}
	if after.vectors != before.vectors || after.vectorWriteGeneration != before.vectorWriteGeneration || after.milvusVectors != before.milvusVectors {
		t.Fatalf("outbox loss wrote vectors: vectors %d to %d, write generation %d to %d, milvus %d to %d",
			before.vectors, after.vectors, before.vectorWriteGeneration, after.vectorWriteGeneration, before.milvusVectors, after.milvusVectors)
	}
	if after.deliveredBatches != 0 || after.pendingBatches != 0 || after.appliedProjections != 1 {
		t.Fatalf("outbox loss delivered/pending batches/applied projections = %d/%d/%d, want 0/0/1",
			after.deliveredBatches, after.pendingBatches, after.appliedProjections)
	}
	if len(after.committedFields) != len(before.committedFields) {
		t.Fatalf("outbox loss committed fields = %d, want %d", len(after.committedFields), len(before.committedFields))
	}
	for fieldKey, digest := range after.committedFields {
		if digest != embeddedUnknownFieldDigest {
			t.Fatalf("reconciled field %s digest = %q, want %q", fieldKey, digest, embeddedUnknownFieldDigest)
		}
	}
	store := scenario.worker.embedded.store
	blocked, err := store.outbox.blockedOwners(t.Context(), store.namespace.ID)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("outbox loss blocked owners = %q, %v, want none", blocked, err)
	}
	listed, err := store.library.ListOwnerOccurrences(t.Context(), store.namespace.ID, liveOwnerID)
	if err != nil || listed.ProjectionOrder != 2 {
		t.Fatalf("outbox loss library projection order = %d, %v, want 2", listed.ProjectionOrder, err)
	}
	if unarchived := countLiveEffectiveScalars(t, scenario.semantic.CatalogPath, embeddedScalarArchived, false); unarchived != len(after.rows) {
		t.Fatalf("outbox loss unarchived rows = %d, want %d", unarchived, len(after.rows))
	}
}

// countLiveEffectiveScalars counts the rows with a reprojected Bool value for
// column in the catalog.
func countLiveEffectiveScalars(t *testing.T, catalogPath string, column string, value bool) int {
	t.Helper()
	catalog := openLiveReadOnly(t, catalogPath)
	var count int
	if err := catalog.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM effective_scalars WHERE column_name = ? AND bool_value = ?`, column, value,
	).Scan(&count); err != nil {
		t.Fatalf("count effective %s scalars: %v", column, err)
	}
	return count
}

// liveScenario is one catalog, outbox, and Milvus collection with a sync
// worker over the shared index.
type liveScenario struct {
	semantic config.ConversationSemanticConfig
	worker   *conversationSemanticSyncWorker
}

func newLiveScenario(t *testing.T, database string, storeRoot string, name string, index *conversation.Index) *liveScenario {
	t.Helper()
	semantic := config.ConversationSemanticConfig{
		IngestionEnabled:        true,
		CollectionID:            liveCollectionID,
		Backend:                 config.ConversationSemanticBackendEmbedded,
		CatalogPath:             filepath.Join(storeRoot, name, "catalog.sqlite"),
		LockPath:                filepath.Join(storeRoot, name, "catalog.lock"),
		PoolID:                  "live-" + name,
		MilvusAddress:           liveMilvusAddress,
		MilvusDatabase:          database,
		MilvusCollection:        "vectors_" + strings.ReplaceAll(name, "-", "_"),
		EmbeddingBaseURL:        liveEmbeddingBaseURL,
		EmbeddingAPIKeyEnv:      liveEmbeddingAPIKeyEnv,
		EmbeddingModel:          liveEmbeddingModel,
		EmbeddingRevision:       "live-test",
		VectorDimension:         liveEmbeddingDimension,
		Normalization:           "l2",
		EmbeddingRequestTimeout: config.Duration(2 * time.Minute),
	}
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.embedded = newEmbeddedConversationSync(semantic, conversationSemanticOutboxPath(semantic.PoolID), newEmbeddedSemanticStatus(), index)
	return &liveScenario{semantic: semantic, worker: worker}
}

func (scenario *liveScenario) runPass(t *testing.T) liveSnapshot {
	t.Helper()
	if err := scenario.worker.runPass(t.Context()); err != nil {
		t.Fatalf("run embedded pass for %s: %v", scenario.semantic.PoolID, err)
	}
	return scenario.snapshot(t)
}

// stageWithoutAcknowledgment builds the conversation generation with the
// worker functions, records it in the outbox, and stages it. With commit it
// also commits the generation and returns the receipt fingerprint. The
// caller then closes the scenario without an outbox acknowledgment.
func (scenario *liveScenario) stageWithoutAcknowledgment(t *testing.T, commit bool) string {
	t.Helper()
	ctx := t.Context()
	store, err := scenario.worker.embedded.ensureStore(ctx, scenario.worker.log)
	if err != nil {
		t.Fatalf("open store for %s: %v", scenario.semantic.PoolID, err)
	}
	stamped, err := scenario.worker.index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	var stats embeddedSyncStats
	candidates := scenario.worker.embeddedCandidates(stamped, &stats)
	if len(candidates) != 1 || candidates[0].record.ID != liveOwnerID {
		t.Fatalf("candidates = %d, want only %s", len(candidates), liveOwnerID)
	}
	generation, _, err := scenario.worker.buildEmbeddedGeneration(ctx, store, candidates[0], &stats)
	if err != nil || generation == nil {
		t.Fatalf("build generation = %v, %v, want a generation", generation, err)
	}
	if err := store.outbox.recordBatch(ctx, generation.batch, generation.rows); err != nil {
		t.Fatalf("record batch: %v", err)
	}
	if _, err := store.delivery.stage(ctx, *generation); err != nil {
		t.Fatalf("stage generation: %v", err)
	}
	if !commit {
		return ""
	}
	receipt, err := store.delivery.commit(ctx, *generation)
	if err != nil {
		t.Fatalf("commit generation: %v", err)
	}
	return receipt.Fingerprint
}

func (scenario *liveScenario) close(t *testing.T) {
	t.Helper()
	scenario.worker.embedded.closeStore(t.Context())
	if scenario.worker.embedded.store != nil {
		t.Fatalf("close store for %s failed", scenario.semantic.PoolID)
	}
}

// liveSnapshot is the committed state of one scenario: the library owner
// state, the outbox batches and committed fields, the catalog occurrences and
// vectors, and the Milvus vector count.
type liveSnapshot struct {
	owner                 liveOwnerState
	deliveredBatches      int
	pendingBatches        int
	receipts              map[string]string
	committedFields       map[string]string
	rows                  map[string]string
	vectors               int
	vectorWriteGeneration int
	milvusVectors         int64
	appliedProjections    int
}

type liveOwnerState struct {
	GenerationOrder uint64
	Fingerprint     string
}

func (scenario *liveScenario) snapshot(t *testing.T) liveSnapshot {
	t.Helper()
	store := scenario.worker.embedded.store
	if store == nil {
		t.Fatalf("store for %s is not open", scenario.semantic.PoolID)
	}
	state, err := store.library.GetOwnerState(t.Context(), liveCollectionID, liveOwnerID)
	if err != nil {
		t.Fatalf("read owner state: %v", err)
	}
	snapshot := liveSnapshot{
		owner:                 liveOwnerState{GenerationOrder: state.GenerationOrder, Fingerprint: state.Fingerprint},
		deliveredBatches:      0,
		pendingBatches:        0,
		receipts:              make(map[string]string),
		committedFields:       make(map[string]string),
		rows:                  make(map[string]string),
		vectors:               0,
		vectorWriteGeneration: 0,
		milvusVectors:         0,
		appliedProjections:    0,
	}
	outbox := openLiveReadOnly(t, conversationSemanticOutboxPath(scenario.semantic.PoolID))
	snapshot.deliveredBatches = queryLiveCount(t, outbox, `SELECT COUNT(*) FROM batches WHERE state = 'delivered'`)
	snapshot.pendingBatches = queryLiveCount(t, outbox, `SELECT COUNT(*) FROM batches WHERE state = 'pending'`)
	snapshot.appliedProjections = queryLiveCount(t, outbox, `SELECT COUNT(*) FROM projections WHERE state = 'applied'`)
	queryLivePairs(t, outbox, `SELECT CAST(generation_order AS TEXT), receipt_fingerprint FROM batches WHERE state = 'delivered'`, snapshot.receipts)
	queryLivePairs(t, outbox, `SELECT field_key, digest FROM committed_fields`, snapshot.committedFields)
	catalog := openLiveReadOnly(t, scenario.semantic.CatalogPath)
	queryLivePairs(t, catalog, `SELECT row_key, occurrence_hash FROM occurrences`, snapshot.rows)
	snapshot.vectors = queryLiveCount(t, catalog, `SELECT COUNT(*) FROM vectors`)
	snapshot.vectorWriteGeneration = queryLiveCount(t, catalog,
		`SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM store_identity WHERE key = 'vector_write_generation'), 0)`)
	snapshot.milvusVectors = countLiveMilvusVectors(t, scenario.semantic)
	return snapshot
}

// assertLiveSnapshotsEqual requires equal owner state, outbox state, catalog
// rows, catalog vectors, vector write generation, and Milvus vector count. An
// equal vector count and write generation show zero embeddings and zero
// vector writes.
func assertLiveSnapshotsEqual(t *testing.T, label string, want liveSnapshot, got liveSnapshot) {
	t.Helper()
	if got.owner != want.owner || got.deliveredBatches != want.deliveredBatches || got.pendingBatches != want.pendingBatches {
		t.Fatalf("%s owner/delivered/pending = %+v/%d/%d, want %+v/%d/%d", label,
			got.owner, got.deliveredBatches, got.pendingBatches, want.owner, want.deliveredBatches, want.pendingBatches)
	}
	if got.vectors != want.vectors || got.vectorWriteGeneration != want.vectorWriteGeneration || got.milvusVectors != want.milvusVectors {
		t.Fatalf("%s vectors/write generation/milvus = %d/%d/%d, want %d/%d/%d", label,
			got.vectors, got.vectorWriteGeneration, got.milvusVectors, want.vectors, want.vectorWriteGeneration, want.milvusVectors)
	}
	if !maps.Equal(got.rows, want.rows) || !maps.Equal(got.committedFields, want.committedFields) || !maps.Equal(got.receipts, want.receipts) {
		t.Fatalf("%s rows, committed fields, or receipts changed", label)
	}
}

// assertLiveCommittedEqual compares the owner state, rows, receipts, and
// committed fields of a recovered scenario with the uninterrupted scenario.
// Both scenarios use the same namespace, owner, generation order, and
// manifest. The receipt fingerprint covers those four values.
func assertLiveCommittedEqual(t *testing.T, label string, want liveSnapshot, got liveSnapshot) {
	t.Helper()
	if got.owner != want.owner || got.deliveredBatches != 1 || got.pendingBatches != 0 {
		t.Fatalf("%s owner/delivered/pending = %+v/%d/%d, want %+v/1/0", label, got.owner, got.deliveredBatches, got.pendingBatches, want.owner)
	}
	if !maps.Equal(got.rows, want.rows) || !maps.Equal(got.committedFields, want.committedFields) || !maps.Equal(got.receipts, want.receipts) {
		t.Fatalf("%s committed rows, committed fields, or receipts differ from the uninterrupted run", label)
	}
	if got.milvusVectors != int64(got.vectors) {
		t.Fatalf("%s Milvus vectors %d differ from catalog vectors %d", label, got.milvusVectors, got.vectors)
	}
}

// assertLiveAppendedRows requires every earlier row with its earlier content
// and requires every new row to belong to an appended message.
func assertLiveAppendedRows(t *testing.T, before map[string]string, after map[string]string) {
	t.Helper()
	for rowKey, occurrenceHash := range before {
		if after[rowKey] != occurrenceHash {
			t.Fatalf("row %s changed or disappeared after the append", rowKey)
		}
	}
	added := 0
	for rowKey := range after {
		if _, existed := before[rowKey]; existed {
			continue
		}
		added++
		if !strings.Contains(rowKey, "/m4/") && !strings.Contains(rowKey, "/m5/") {
			t.Fatalf("append added row %s outside the appended messages", rowKey)
		}
	}
	if added != 2 {
		t.Fatalf("append added %d rows, want 2", added)
	}
}

// createLiveMilvusDatabase creates a database named clyde_live_ followed by
// 32 random hex characters, fails when the name exists, and drops the
// database with its collections when the test ends.
func createLiveMilvusDatabase(t *testing.T) string {
	t.Helper()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate database name: %v", err)
	}
	name := liveMilvusDatabasePrefix + hex.EncodeToString(random)
	ctx := t.Context()
	admin, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: liveMilvusAddress})
	if err != nil {
		t.Fatalf("connect to Milvus at %s: %v", liveMilvusAddress, err)
	}
	existing, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil {
		t.Fatalf("list Milvus databases: %v", err)
	}
	if slices.Contains(existing, name) {
		t.Fatalf("Milvus database %s already exists", name)
	}
	if err := admin.CreateDatabase(ctx, milvusclient.NewCreateDatabaseOption(name)); err != nil {
		t.Fatalf("create Milvus database %s: %v", name, err)
	}
	t.Logf("created Milvus database %s at %s", name, time.Now().UTC().Format(time.RFC3339))
	t.Cleanup(func() {
		dropLiveMilvusDatabase(t, admin, name)
	})
	return name
}

func dropLiveMilvusDatabase(t *testing.T, admin *milvusclient.Client, name string) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	scoped, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: liveMilvusAddress, DBName: name})
	if err != nil {
		t.Errorf("connect to Milvus database %s for cleanup: %v", name, err)
		return
	}
	collections, err := scoped.ListCollections(ctx, milvusclient.NewListCollectionOption())
	if err != nil {
		t.Errorf("list collections of %s: %v", name, err)
	}
	for _, collection := range collections {
		if err := scoped.DropCollection(ctx, milvusclient.NewDropCollectionOption(collection)); err != nil {
			t.Errorf("drop collection %s of %s: %v", collection, name, err)
		}
	}
	if err := scoped.Close(ctx); err != nil {
		t.Errorf("close Milvus client for %s: %v", name, err)
	}
	if err := admin.DropDatabase(ctx, milvusclient.NewDropDatabaseOption(name)); err != nil {
		t.Errorf("drop Milvus database %s: %v", name, err)
	}
	remaining, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil || slices.Contains(remaining, name) {
		t.Errorf("Milvus database %s remains after drop: %v", name, err)
	}
	t.Logf("dropped Milvus database %s with collections %v at %s", name, collections, time.Now().UTC().Format(time.RFC3339))
	if err := admin.Close(ctx); err != nil {
		t.Errorf("close Milvus admin client: %v", err)
	}
}

func countLiveMilvusVectors(t *testing.T, semantic config.ConversationSemanticConfig) int64 {
	t.Helper()
	ctx := t.Context()
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: semantic.MilvusAddress, DBName: semantic.MilvusDatabase})
	if err != nil {
		t.Fatalf("connect to Milvus database %s: %v", semantic.MilvusDatabase, err)
	}
	defer func() {
		if err := client.Close(ctx); err != nil {
			t.Errorf("close Milvus client: %v", err)
		}
	}()
	result, err := client.Query(ctx, milvusclient.NewQueryOption(semantic.MilvusCollection).
		WithOutputFields("count(*)").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("count vectors in %s: %v", semantic.MilvusCollection, err)
	}
	count, err := result.GetColumn("count(*)").GetAsInt64(0)
	if err != nil {
		t.Fatalf("read vector count of %s: %v", semantic.MilvusCollection, err)
	}
	return count
}

func openLiveReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func queryLiveCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), query).Scan(&count); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return count
}

func queryLivePairs(t *testing.T, db *sql.DB, query string, into map[string]string) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		into[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
}

func ageLiveArtifact(t *testing.T, path string) {
	t.Helper()
	aged := time.Now().Add(-liveArtifactAge)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

func refreshLiveIndex(t *testing.T, index *conversation.Index) {
	t.Helper()
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
}

func requireLiveOwner(t *testing.T, index *conversation.Index) {
	t.Helper()
	stamped, err := index.ListWithStamps(t.Context())
	if err != nil {
		t.Fatalf("list records: %v", err)
	}
	if len(stamped) != 1 || stamped[0].Record.ID != liveOwnerID {
		t.Fatalf("records = %d, want only %s", len(stamped), liveOwnerID)
	}
}
