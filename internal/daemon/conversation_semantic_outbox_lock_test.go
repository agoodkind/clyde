package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/lm-semantic-search/library/embedded"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

// openTestOutbox takes the outbox lock and opens the outbox at path, the way
// the embedded worker opens it.
func openTestOutbox(ctx context.Context, path string) (*conversationSemanticOutbox, error) {
	lock, err := lockConversationSemanticOutbox(ctx, path)
	if err != nil {
		return nil, err
	}
	outbox, err := openLockedConversationSemanticOutbox(ctx, path, lock)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	return outbox, nil
}

// TestConversationSemanticOutboxOpensOnce opens the outbox of one pool and
// opens it again while the first outbox is open, the way a replacement daemon
// worker opens it during a reload before the old worker drains. The second
// open must fail with errOutboxOwned. After the first outbox closes, a new
// open must succeed.
func TestConversationSemanticOutboxOpensOnce(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	path := conversationSemanticOutboxPath("lock-test")
	first, err := openTestOutbox(t.Context(), path)
	if err != nil {
		t.Fatalf("open first outbox: %v", err)
	}
	second, err := openTestOutbox(t.Context(), path)
	if err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("second outbox opened while the first outbox was open")
	}
	if !errors.Is(err, errOutboxOwned) {
		_ = first.Close()
		t.Fatalf("second open error = %v, want errOutboxOwned", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first outbox: %v", err)
	}
	third, err := openTestOutbox(t.Context(), path)
	if err != nil {
		t.Fatalf("open outbox after close: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("close outbox: %v", err)
	}
}

// TestEmbeddedWorkerOpensNothingWhileOutboxLocked opens the outbox of one pool
// as the old worker during a reload and runs a sync pass of a second embedded
// worker for the same pool. The second worker opens its store with the
// library embedded vector store. While the first outbox is open, the pass must
// fail with errOutboxOwned, call no store open, and create no library catalog.
// After the first outbox closes, the next pass must open the store and
// succeed.
func TestEmbeddedWorkerOpensNothingWhileOutboxLocked(t *testing.T) {
	isolateEmbeddedProjectionStores(t)
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	semantic := config.ConversationSemanticConfig{
		IngestionEnabled:  true,
		CollectionID:      "clyde-conversations",
		Backend:           config.ConversationSemanticBackendEmbedded,
		CatalogPath:       filepath.Join(root, "catalog", "catalog.sqlite"),
		LockPath:          filepath.Join(root, "catalog", "catalog.lock"),
		PoolID:            "lock-worker-test",
		EmbeddingBaseURL:  "http://localhost:1/v1",
		EmbeddingModel:    "nvidia/NV-EmbedCode-7b-v1",
		EmbeddingRevision: "lock-worker-test",
		VectorDimension:   4096,
		Normalization:     "l2",
	}
	outboxPath := conversationSemanticOutboxPath(semantic.PoolID)
	oldWorkerOutbox, err := openTestOutbox(t.Context(), outboxPath)
	if err != nil {
		t.Fatalf("open the old worker outbox: %v", err)
	}
	index := conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{})
	if err := index.Refresh(t.Context()); err != nil {
		t.Fatalf("refresh index: %v", err)
	}
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.embedded = newEmbeddedConversationSync(semantic, outboxPath, newEmbeddedSemanticStatus(), index)
	storeOpens := 0
	worker.embedded.open = func(ctx context.Context, lock *os.File, log *slog.Logger) (*embeddedConversationStore, error) {
		storeOpens++
		vectors, err := embedded.New(embedded.Config{Root: filepath.Join(root, "vectors")})
		if err != nil {
			return nil, err
		}
		embedder, err := newEmbeddedConversationEmbedder(ctx, semantic)
		if err != nil {
			return nil, err
		}
		return openEmbeddedConversationLibrary(ctx, semantic, outboxPath, lock, vectors, embedder, log)
	}
	t.Cleanup(func() { worker.embedded.closeStore(context.Background()) })

	err = worker.runPass(t.Context())
	_, catalogErr := os.Stat(semantic.CatalogPath)
	if !errors.Is(err, errOutboxOwned) || storeOpens != 0 || !os.IsNotExist(catalogErr) {
		_ = oldWorkerOutbox.Close()
		t.Fatalf("pass while locked error/store opens/catalog stat = %v/%d/%v, want errOutboxOwned/0/not exist", err, storeOpens, catalogErr)
	}
	if err := oldWorkerOutbox.Close(); err != nil {
		t.Fatalf("close the old worker outbox: %v", err)
	}
	err = worker.runPass(t.Context())
	_, catalogErr = os.Stat(semantic.CatalogPath)
	if err != nil || storeOpens != 1 || catalogErr != nil || worker.embedded.store == nil {
		t.Fatalf("pass after release error/store opens/catalog stat = %v/%d/%v, want nil/1/nil with an open store", err, storeOpens, catalogErr)
	}
}
