//go:build live

package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
)

func TestEmbeddedOwnerAliasMetadataUsesIngestionSource(t *testing.T) {
	requireLiveLocalEmbeddingModel(t)
	stores := isolateEmbeddedProjectionStores(t)
	primary := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, primary)
	alias := filepath.Join(stores.codexHome, "sessions", "2026", "05", "01", filepath.Base(primary))
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, line := range embeddedProjectionCodexInitialLines {
		appendEmbeddedProjectionLines(t, alias, []string{strings.ReplaceAll(line, `"cwd":"/repo"`, `"cwd":"/older-repo"`)})
	}
	older := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(alias, older, older); err != nil {
		t.Fatal(err)
	}
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	records, err := index.ListAllWithStamps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Record.ID != records[1].Record.ID || records[0].Record.ArtifactPath != primary {
		t.Fatalf("discovered records = %+v, want newer primary and older alias under one ID", records)
	}
	store, semantic := openEmbeddedQueryTestStore(t)
	if err := store.outbox.releaseLock(); err != nil {
		t.Fatal(err)
	}
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.embedded = newEmbeddedConversationSync(semantic, store.outbox.path, newEmbeddedSemanticStatus(), index)
	worker.embedded.store = store
	source := &embeddedConversationSearchSource{library: store.library, semantic: semantic, gate: nil, index: index, outbox: store.outbox}
	for pass := range 3 {
		if err := worker.runPass(t.Context()); err != nil {
			t.Fatalf("ingestion pass %d: %v", pass, err)
		}
		for _, workspace := range []string{"/repo", "/older-repo"} {
			result, err := source.SearchConversations(t.Context(), conversation.SearchConversationsOptions{
				Query: "run the test suite", WorkspaceRoot: workspace, Roles: []string{"user"}, Limit: 10,
			})
			if err != nil {
				t.Fatalf("pass %d workspace %s: %v", pass, workspace, err)
			}
			want := 0
			if workspace == "/repo" {
				want = 1
			}
			if len(result.Matches) != want {
				t.Fatalf("pass %d workspace %s matched %d, want %d", pass, workspace, len(result.Matches), want)
			}
		}
	}
	metadata, err := store.outbox.ownerMetadata(t.Context(), semantic.CollectionID)
	if err != nil {
		t.Fatal(err)
	}
	if metadata[liveOwnerID].ProjectionOrder != 0 {
		t.Fatalf("unchanged source metadata projection order = %d, want zero", metadata[liveOwnerID].ProjectionOrder)
	}
}
