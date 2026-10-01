//go:build live

package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func TestEmbeddedCompatibleAliasPublishesMissingFields(t *testing.T) {
	requireLiveLocalEmbeddingModel(t)
	stores := isolateEmbeddedProjectionStores(t)
	primary := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, primary)
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	database := createLiveMilvusDatabase(t)
	root := t.TempDir()
	scenario := newLiveScenario(t, database, root, "compatible-alias", index)
	logPath := filepath.Join(root, "alias-operations.jsonl")
	if registry := os.Getenv("CLYDE_LIVE_DATABASE_REGISTRY"); registry != "" {
		logPath = filepath.Join(filepath.Dir(registry), "alias-operations.jsonl")
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := logFile.Close(); err != nil {
			t.Error(err)
		}
	})
	logger := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug}))
	scenario.worker.log = logger
	t.Cleanup(func() { scenario.close(t) })
	before := scenario.runPass(t)
	if before.owner.GenerationOrder != 1 || len(before.rows) != 3 {
		t.Fatalf("initial owner = %+v, rows=%d", before.owner, len(before.rows))
	}
	alias := filepath.Join(stores.codexHome, "sessions", "2026", "05", "01", filepath.Base(primary))
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	appendEmbeddedProjectionLines(t, alias, embeddedProjectionCodexInitialLines)
	appendEmbeddedProjectionLines(t, alias, embeddedProjectionCodexAppendedLines)
	ageLiveArtifact(t, alias)
	refreshLiveIndex(t, index)
	after := scenario.runPass(t)
	if after.owner.GenerationOrder != 2 || len(after.rows) != 5 || len(after.committedFields) != 5 {
		t.Fatalf("extended owner = %+v, rows=%d fields=%d", after.owner, len(after.rows), len(after.committedFields))
	}
	assertLiveAppendedRows(t, before.rows, after.rows)
	store := scenario.worker.embedded.store
	searchSemantic := scenario.semantic
	searchSemantic.SearchEnabled = true
	source := &embeddedConversationSearchSource{library: store.library, semantic: searchSemantic, gate: nil, index: index, outbox: store.outbox}
	page, err := source.SearchConversations(t.Context(), conversation.SearchConversationsOptions{Query: "now run the linter", Roles: []string{"user"}, Limit: 10})
	if err != nil || len(page.Matches) != 2 {
		t.Fatalf("extended public search = %+v, %v", page, err)
	}
	assertLiveSnapshotsEqual(t, "unchanged aliases", after, scenario.runPass(t))
	assertEmbeddedAliasZeroOperations(t, logPath)
	scenario.close(t)
	scenario = newLiveScenario(t, database, root, "compatible-alias", index)
	scenario.worker.log = logger
	assertLiveSnapshotsEqual(t, "restarted aliases", after, scenario.runPass(t))
	assertEmbeddedAliasZeroOperations(t, logPath)
	if err := os.Remove(primary); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	freshness := newConversationSemanticFreshness()
	scenario.worker.freshness = freshness
	assertLiveSnapshotsEqual(t, "missing accepted source", after, scenario.runPass(t))
	server := &controlServer{freshness: freshness.snapshot}
	status, err := server.GetSemanticSearchFreshness(t.Context(), &clydev1.GetSemanticSearchFreshnessRequest{})
	if err != nil || status.GetSemanticFreshness().GetPending() != 1 {
		t.Fatalf("missing accepted source freshness = %+v, %v", status, err)
	}
}

func assertEmbeddedAliasZeroOperations(t *testing.T, logPath string) {
	t.Helper()
	proof, err := readFrozenPassProof(logPath)
	if err != nil || proof.EmbeddingAttempts != 0 || proof.RequestedInputs != 0 || proof.VectorCalls != 0 || proof.Stages != 0 || proof.FailedOperations != 0 {
		t.Fatalf("unchanged alias operations = %+v, %v", proof, err)
	}
	t.Logf("completed alias run %s: embedding=0 upsert=0 stage=0 catalog_transactions=%d", proof.RunID, proof.CatalogTransactions)
}

func TestEmbeddedDivergentAliasRetainsPublishedRows(t *testing.T) {
	requireLiveLocalEmbeddingModel(t)
	stores := isolateEmbeddedProjectionStores(t)
	primary := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, primary)
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	scenario := newLiveScenario(t, createLiveMilvusDatabase(t), t.TempDir(), "divergent-alias", index)
	t.Cleanup(func() { scenario.close(t) })
	before := scenario.runPass(t)
	alias := filepath.Join(stores.codexHome, "sessions", "2026", "05", "01", filepath.Base(primary))
	if err := os.MkdirAll(filepath.Dir(alias), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, line := range embeddedProjectionCodexInitialLines {
		appendEmbeddedProjectionLines(t, alias, []string{strings.Replace(line, "2026-05-02T17:09:05.000Z", "2026-05-02T17:09:05.001Z", 1)})
	}
	appendEmbeddedProjectionLines(t, alias, embeddedProjectionCodexAppendedLines)
	ageLiveArtifact(t, alias)
	refreshLiveIndex(t, index)
	freshness := newConversationSemanticFreshness()
	scenario.worker.freshness = freshness
	server := &controlServer{freshness: freshness.snapshot}
	for retry := range failedLoadSuppressThreshold + 1 {
		assertLiveSnapshotsEqual(t, "divergent alias", before, scenario.runPass(t))
		status, err := server.GetSemanticSearchFreshness(t.Context(), &clydev1.GetSemanticSearchFreshnessRequest{})
		if err != nil || status.GetSemanticFreshness().GetPending() != 1 {
			t.Fatalf("retry %d divergent freshness = %+v, %v", retry, status, err)
		}
	}
}
