package daemon

import (
	"database/sql"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"goodkind.io/lm-semantic-search/library"
	"goodkind.io/lm-semantic-search/library/embedded"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

const (
	blockedBatchOwnerID      = "codex:blocked-batch-owner"
	blockedProjectionOwnerID = "codex:blocked-projection-owner"
	ownerBlockedMessage      = "daemon.conversation_semantic_embedded.owner_blocked"
)

// TestEmbeddedOwnerBlockedOnStaleGeneration opens the library through the
// daemon open path with the embedded vector store and records a pending batch
// at generation order 1 and a pending projection at order 1. It then commits
// generation order 2 and applies projection order 2 through the library
// directly. The replay receives a real library.ErrStaleGeneration for both
// items. Each item must move to the blocked state with the error class and
// the library order, log one owner_blocked record, and stay out of every
// later replay. No step embeds text, and the embedding endpoint is a closed
// local port.
func TestEmbeddedOwnerBlockedOnStaleGeneration(t *testing.T) {
	store, capture := openBlockedTestStore(t)
	ctx := t.Context()

	generation := recordBlockedTestBatch(t, store)
	if generation.batch.GenerationOrder != 1 {
		t.Fatalf("recorded batch order = %d, want 1", generation.batch.GenerationOrder)
	}
	external, err := store.library.Apply(ctx, library.Batch{
		Namespace:        store.namespace.ID,
		OwnerID:          blockedBatchOwnerID,
		GenerationOrder:  2,
		IdempotencyToken: "external-generation-2",
		Mode:             library.Append,
		Rows:             nil,
	})
	if err != nil || external.GenerationOrder != 2 {
		t.Fatalf("commit external generation 2 = %+v, %v", external, err)
	}
	projection := embeddedOutboxProjection{
		Namespace:       store.namespace.ID,
		OwnerID:         blockedProjectionOwnerID,
		ProjectionOrder: 1,
		Token:           "projection-1",
		Metadata:        embeddedOwnerMetadata{Provider: "codex", WorkspaceRoot: "", Archived: true, Subagent: false},
	}
	if err := store.outbox.recordProjection(ctx, projection, nil); err != nil {
		t.Fatalf("record projection: %v", err)
	}
	if _, err := store.library.ReprojectScalars(ctx, library.ScalarProjection{
		Namespace:        store.namespace.ID,
		OwnerID:          blockedProjectionOwnerID,
		ProjectionOrder:  2,
		IdempotencyToken: "external-projection-2",
		Rows:             nil,
	}); err != nil {
		t.Fatalf("apply external projection 2: %v", err)
	}

	for attempt := range 2 {
		replay, err := store.delivery.replayPending(ctx)
		if err != nil {
			t.Fatalf("attempt %d: replay batches: %v", attempt, err)
		}
		replayedProjections, _, err := store.delivery.replayPendingProjections(ctx)
		if err != nil {
			t.Fatalf("attempt %d: replay projections: %v", attempt, err)
		}
		if replay.replayed != 0 || replayedProjections != 0 {
			t.Fatalf("attempt %d: replayed batches/projections = %d/%d, want 0/0", attempt, replay.replayed, replayedProjections)
		}
	}

	outbox := openLiveReadOnlyForBlocked(t, conversationSemanticOutboxPath("blocked-test"))
	assertBlockedRow(t, outbox, `SELECT state, blocked_class, blocked_library_order, blocked_unix FROM batches WHERE batch_id = ?`,
		generation.batch.BatchID, 2)
	assertBlockedRow(t, outbox, `SELECT state, blocked_class, blocked_library_order, blocked_unix FROM projections WHERE owner_id = ?`,
		blockedProjectionOwnerID, 0)
	blocked, err := store.outbox.blockedOwners(ctx, store.namespace.ID)
	if err != nil || !slices.Equal(blocked, []string{blockedBatchOwnerID, blockedProjectionOwnerID}) {
		t.Fatalf("blocked owners = %q, %v, want both owners", blocked, err)
	}
	blockedRecords := 0
	for _, message := range embeddedGateMessages(capture) {
		if message == ownerBlockedMessage {
			blockedRecords++
		}
	}
	if blockedRecords != 2 {
		t.Fatalf("owner_blocked records = %d, want one for the batch and one for the projection", blockedRecords)
	}
}

func openBlockedTestStore(t *testing.T) (*embeddedConversationStore, *semanticLogCapture) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	semantic := config.ConversationSemanticConfig{
		IngestionEnabled:  true,
		CollectionID:      "clyde-conversations",
		Backend:           config.ConversationSemanticBackendEmbedded,
		CatalogPath:       filepath.Join(root, "catalog", "catalog.sqlite"),
		LockPath:          filepath.Join(root, "catalog", "catalog.lock"),
		PoolID:            "blocked-test",
		EmbeddingBaseURL:  "http://localhost:1/v1",
		EmbeddingModel:    "nvidia/NV-EmbedCode-7b-v1",
		EmbeddingRevision: "blocked-test",
		VectorDimension:   4096,
		Normalization:     "l2",
	}
	vectors, err := embedded.New(embedded.Config{Root: filepath.Join(root, "vectors")})
	if err != nil {
		t.Fatalf("create embedded vector store: %v", err)
	}
	embedder, err := newEmbeddedConversationEmbedder(t.Context(), semantic)
	if err != nil {
		t.Fatalf("create embedder: %v", err)
	}
	capture := &semanticLogCapture{mu: sync.Mutex{}, records: nil}
	store, err := openEmbeddedConversationLibrary(t.Context(), semantic, conversationSemanticOutboxPath(semantic.PoolID), vectors, embedder, slog.New(capture))
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() {
		if err := store.library.Close(); err != nil {
			t.Errorf("close library: %v", err)
		}
		if err := store.outbox.Close(); err != nil {
			t.Errorf("close outbox: %v", err)
		}
	})
	return store, capture
}

// recordBlockedTestBatch prepares one chat field of the batch owner at the
// next library order and records it in the outbox without staging it.
func recordBlockedTestBatch(t *testing.T, store *embeddedConversationStore) embeddedGeneration {
	t.Helper()
	ctx := t.Context()
	projected := searchbackend.ProjectFields(searchbackend.Conversation{
		ID:                     blockedBatchOwnerID,
		LoadRules:              conversation.LoadRulesTag(defaultSemanticContentKinds()),
		MessageCount:           1,
		TrailingMessageMayGrow: false,
		ArtifactSettled:        true,
		ToolDetail:             embeddedToolDetail(defaultSemanticContentKinds()),
	}, []searchbackend.Message{embeddedOccurrenceChatMessage("blocked owner text")})
	record := conversation.Record{ID: blockedBatchOwnerID, Provider: conversation.ProviderCodex}
	owner := newEmbeddedConversationOwner(record, defaultSemanticContentKinds())
	rows, err := embeddedOutboxRows(ctx, store.namespace, owner, projected.Fields)
	if err != nil {
		t.Fatalf("build rows: %v", err)
	}
	generation, err := store.delivery.prepareGeneration(ctx, embeddedOutboxBatch{
		BatchID:           "",
		Namespace:         store.namespace.ID,
		OwnerID:           blockedBatchOwnerID,
		GenerationOrder:   0,
		SourcePath:        "/rollout.jsonl",
		SourceStamp:       "1:1",
		ProjectionProfile: owner.ProjectionProfile,
		RowCount:          0,
		ManifestHash:      "",
		Metadata:          embeddedOwnerMetadataOf(record),
	}, rows)
	if err != nil {
		t.Fatalf("prepare generation: %v", err)
	}
	if err := store.outbox.recordBatch(ctx, generation.batch, generation.rows); err != nil {
		t.Fatalf("record batch: %v", err)
	}
	return generation
}

func openLiveReadOnlyForBlocked(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertBlockedRow(t *testing.T, db *sql.DB, query string, key string, wantLibraryOrder uint64) {
	t.Helper()
	var state, class string
	var libraryOrder uint64
	var blockedUnix int64
	if err := db.QueryRowContext(t.Context(), query, key).Scan(&state, &class, &libraryOrder, &blockedUnix); err != nil {
		t.Fatalf("read outbox item %s: %v", key, err)
	}
	if state != embeddedOutboxStateBlocked || class != string(embeddedBlockedStaleGeneration) || libraryOrder != wantLibraryOrder ||
		blockedUnix < time.Now().Add(-time.Hour).Unix() {
		t.Fatalf("outbox item %s state/class/library order/time = %s/%s/%d/%d, want blocked/stale_generation/%d/recent",
			key, state, class, libraryOrder, blockedUnix, wantLibraryOrder)
	}
}
