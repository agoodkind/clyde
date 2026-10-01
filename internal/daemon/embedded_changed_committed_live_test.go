//go:build live

package daemon

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/milvusclient"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func TestEmbeddedChangedCommittedSourceRemainsPending(t *testing.T) {
	requireLiveLocalEmbeddingModel(t)
	stores := isolateEmbeddedProjectionStores(t)
	artifact := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, artifact)
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	database := ""
	t.Cleanup(func() { verifyChangedFieldDatabaseDeleted(t, database) })
	store, semantic := openEmbeddedQueryTestStore(t)
	database = semantic.MilvusDatabase
	if err := store.outbox.releaseLock(); err != nil {
		t.Fatal(err)
	}
	freshness := newConversationSemanticFreshness()
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.freshness = freshness
	worker.embedded = newEmbeddedConversationSync(semantic, store.outbox.path, newEmbeddedSemanticStatus(), index)
	worker.embedded.store = store
	server := &controlServer{freshness: freshness.snapshot}
	source := &embeddedConversationSearchSource{library: store.library, semantic: semantic, gate: nil, index: index, outbox: store.outbox}
	if err := worker.runPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := store.library.ListOwnerOccurrences(t.Context(), semantic.CollectionID, liveOwnerID)
	if err != nil || len(before.Rows) == 0 {
		t.Fatalf("initial owner = %+v, %v, want committed occurrences", before, err)
	}
	bytes, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(bytes), "run the test suite", "run a different test suite", 1)
	if changed == string(bytes) {
		t.Fatal("source fixture has no selected text to edit")
	}
	if err := os.WriteFile(artifact, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	appendEmbeddedProjectionLines(t, artifact, embeddedProjectionCodexAppendedLines)
	ageLiveArtifact(t, artifact)
	refreshLiveIndex(t, index)
	for retry := range failedLoadSuppressThreshold + 1 {
		if err := worker.runPass(t.Context()); err != nil {
			t.Fatalf("candidate pass %d: %v", retry, err)
		}
		status, err := server.GetSemanticSearchFreshness(t.Context(), &clydev1.GetSemanticSearchFreshnessRequest{})
		if err != nil || status.GetSemanticFreshness().GetPending() != 1 || status.GetSemanticFreshness().GetNeeded() != 1 {
			t.Fatalf("retry %d freshness = %+v, %v, want one pending owner", retry, status, err)
		}
		after, err := store.library.ListOwnerOccurrences(t.Context(), semantic.CollectionID, liveOwnerID)
		if err != nil || len(after.Rows) != len(before.Rows) || after.State.GenerationOrder != before.State.GenerationOrder {
			t.Fatalf("retry %d committed owner = %+v, %v, want prior rows and generation", retry, after, err)
		}
		page, err := source.SearchConversations(t.Context(), conversation.SearchConversationsOptions{
			Query: "run the test suite", Roles: []string{"user"}, Limit: 10,
		})
		if err != nil || len(page.Matches) != 1 || page.Matches[0].Snippet != "run the test suite" {
			t.Fatalf("retry %d stored search = %+v, %v, want original excerpt", retry, page, err)
		}
	}
}

func verifyChangedFieldDatabaseDeleted(t *testing.T, database string) {
	t.Helper()
	if database == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	defer cancel()
	admin, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: liveMilvusAddress})
	if err != nil {
		t.Errorf("verify database cleanup: %v", err)
		return
	}
	defer func() {
		if err := admin.Close(ctx); err != nil {
			t.Errorf("close cleanup verification client: %v", err)
		}
	}()
	databases, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil || slices.Contains(databases, database) {
		t.Errorf("database %s cleanup: %v, remaining=%t", database, err, slices.Contains(databases, database))
		return
	}
	t.Logf("verified deleted isolated database %s", database)
}
