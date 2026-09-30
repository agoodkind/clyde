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

func TestEmbeddedContextUsesAcceptedAlias(t *testing.T) {
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
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.embedded = newEmbeddedConversationSync(semantic, store.outbox.path, newEmbeddedSemanticStatus(), index)
	worker.embedded.store = store
	if err := worker.runPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	source := &embeddedConversationSearchSource{library: store.library, semantic: semantic, gate: nil, index: index, outbox: store.outbox}
	options := conversation.SearchConversationsOptions{Query: "run the test suite", Roles: []string{"user"}, Limit: 10, ContextWindow: 1}
	initial, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(initial.Matches) != 1 || initial.Matches[0].ContextState != conversation.SearchContextStateAvailable {
		t.Fatalf("initial context = %+v, %v, want available", initial, err)
	}
	alias := filepath.Join(stores.codexHome, "sessions", "2026", "05", "03", filepath.Base(artifact))
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	appendEmbeddedProjectionLines(t, alias, embeddedProjectionCodexInitialLines[:1])
	newer := time.Now().Add(-time.Hour)
	if err := os.Chtimes(alias, newer, newer); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	records, err := index.ListAllWithStamps(t.Context())
	if err != nil || len(records) != 2 || records[0].Record.ArtifactPath != alias || records[0].Record.ID != records[1].Record.ID {
		t.Fatalf("alias records = %+v, %v, want newer empty alias under original ID", records, err)
	}
	verified, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(verified.Matches) != 1 || verified.Matches[0].ContextState != conversation.SearchContextStateAvailable || verified.Matches[0].ContextWindow != initial.Matches[0].ContextWindow || verified.Matches[0].Record.ArtifactPath != artifact || verified.Matches[0].Record.NativeID != initial.Matches[0].Record.NativeID {
		t.Fatalf("accepted alias context = %+v, %v, want original verified source", verified, err)
	}
	appendEmbeddedProjectionLines(t, artifact, embeddedProjectionCodexAppendedLines)
	refreshLiveIndex(t, index)
	appended, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(appended.Matches) != 1 || appended.Matches[0].ContextWindow != initial.Matches[0].ContextWindow || appended.Matches[0].ContextState != conversation.SearchContextStateAvailable {
		t.Fatalf("accepted source append = %+v, %v, want unchanged verified window", appended, err)
	}
	contents, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(contents), "run the test suite", "run a different test suite", 1)
	if changed == string(contents) {
		t.Fatal("accepted source fixture has no selected text to edit")
	}
	if err := os.WriteFile(artifact, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	assertAcceptedContextUnavailable(t, source, options, initial.Matches[0].Snippet)
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	assertAcceptedContextUnavailable(t, source, options, initial.Matches[0].Snippet)
}

func assertAcceptedContextUnavailable(t *testing.T, source *embeddedConversationSearchSource, options conversation.SearchConversationsOptions, snippet string) {
	t.Helper()
	page, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].ContextState != conversation.SearchContextStateUnavailable || page.Matches[0].Snippet != snippet {
		t.Fatalf("unverifiable accepted source = %+v, %v, want stored excerpt and unavailable context", page, err)
	}
}
