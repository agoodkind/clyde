package daemon

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"goodkind.io/lm-semantic-search/library"
	"goodkind.io/lm-semantic-search/library/embedded"

	"goodkind.io/clyde/internal/config"
)

// TestEmbeddedConversationLibraryOpens opens the pinned shared search library
// through the daemon open path with a real SQLite catalog and the in-process
// embedded vector store, registers the conversation namespace, and opens the
// outbox. It closes everything and opens the same catalog again. The test
// never embeds text, and its embedding endpoint is a closed local port.
func TestEmbeddedConversationLibraryOpens(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	semantic := config.ConversationSemanticConfig{
		IngestionEnabled:  true,
		CollectionID:      "clyde-conversations",
		Backend:           config.ConversationSemanticBackendEmbedded,
		CatalogPath:       filepath.Join(root, "catalog", "catalog.sqlite"),
		LockPath:          filepath.Join(root, "catalog", "catalog.lock"),
		PoolID:            "open-test",
		EmbeddingBaseURL:  "http://localhost:1/v1",
		EmbeddingModel:    "nvidia/NV-EmbedCode-7b-v1",
		EmbeddingRevision: "open-test",
		VectorDimension:   4096,
		Normalization:     "l2",
	}
	outboxPath := conversationSemanticOutboxPath(semantic.PoolID)
	for attempt := range 2 {
		vectors, err := embedded.New(embedded.Config{Root: filepath.Join(root, "vectors")})
		if err != nil {
			t.Fatalf("attempt %d: create embedded vector store: %v", attempt, err)
		}
		embedder, err := newEmbeddedConversationEmbedder(t.Context(), semantic)
		if err != nil {
			t.Fatalf("attempt %d: create embedder: %v", attempt, err)
		}
		store, err := openLockedTestLibrary(t, semantic, outboxPath, vectors, embedder, slog.Default())
		if err != nil {
			t.Fatalf("attempt %d: open library through the daemon open path: %v", attempt, err)
		}
		state, stateErr := store.library.GetOwnerState(t.Context(), semantic.CollectionID, "codex:none")
		pending, pendingErr := store.outbox.pendingBatches(t.Context())
		changed := embeddedConversationNamespace(semantic.CollectionID)
		changed.Scalars = changed.Scalars[1:]
		changedErr := store.library.RegisterNamespace(t.Context(), changed)
		closeErr := errors.Join(store.library.Close(), store.outbox.Close())
		if stateErr != nil || pendingErr != nil || closeErr != nil {
			t.Fatalf("attempt %d: owner state, pending batches, or close failed: %v", attempt, errors.Join(stateErr, pendingErr, closeErr))
		}
		if state.GenerationOrder != 0 || len(pending) != 0 {
			t.Fatalf("attempt %d: owner order %d and pending batches %d, want an empty store", attempt, state.GenerationOrder, len(pending))
		}
		if !errors.Is(changedErr, library.ErrInvalidRequest) {
			t.Fatalf("attempt %d: changed namespace declaration error = %v, want an invalid request", attempt, changedErr)
		}
	}
}
