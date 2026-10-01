//go:build live

package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func TestEmbeddedColdAliasMetadataDisagreementRemainsPending(t *testing.T) {
	testEmbeddedAliasMetadata(t, false)
}

func TestEmbeddedAcceptedAliasMetadataRetainsStoredValues(t *testing.T) {
	testEmbeddedAliasMetadata(t, true)
}

func testEmbeddedAliasMetadata(t *testing.T, acceptedFirst bool) {
	t.Helper()
	requireLiveLocalEmbeddingModel(t)
	stores := isolateEmbeddedProjectionStores(t)
	primary := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, primary)
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	store, semantic := openEmbeddedQueryTestStore(t)
	if err := store.outbox.releaseLock(); err != nil {
		t.Fatal(err)
	}
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.embedded = newEmbeddedConversationSync(semantic, store.outbox.path, newEmbeddedSemanticStatus(), index)
	worker.embedded.store = store
	freshness := newConversationSemanticFreshness()
	worker.freshness = freshness
	server := &controlServer{freshness: freshness.snapshot}
	if acceptedFirst {
		if err := worker.runPass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
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
	refreshLiveIndex(t, index)
	source := &embeddedConversationSearchSource{library: store.library, semantic: semantic, gate: nil, index: index, outbox: store.outbox}
	for pass := range 3 {
		if err := worker.runPass(t.Context()); err != nil {
			t.Fatalf("ingestion pass %d: %v", pass, err)
		}
		if !acceptedFirst {
			owner, err := store.library.ListOwnerOccurrences(t.Context(), semantic.CollectionID, liveOwnerID)
			if err != nil || owner.State.GenerationOrder != 0 || len(owner.Rows) != 0 {
				t.Fatalf("cold disagreement published owner: %+v, %v", owner, err)
			}
			status, err := server.GetSemanticSearchFreshness(t.Context(), &clydev1.GetSemanticSearchFreshnessRequest{})
			if err != nil || status.GetSemanticFreshness().GetPending() != 1 {
				t.Fatalf("cold disagreement freshness = %+v, %v", status, err)
			}
			continue
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
