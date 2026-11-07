//go:build milvusintegration

package statusproof_test

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/checkpointtest"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	"goodkind.io/clyde/internal/daemon"
)

func waitForParallelProofComplete(t *testing.T, count int64) []daemon.StatusMetric {
	t.Helper()
	return waitForStatusProof(t, "both-complete", ingestionProofFailureTimeout, func(semantic daemon.SemanticStatus) bool {
		for _, backend := range []string{"milvus", "local"} {
			prefix := "semantic_backends." + backend + "."
			embedded, known := statusProofKnownInt(semantic.Detail, prefix+"semantic_freshness.embedded")
			pending, pendingKnown := statusProofKnownInt(semantic.Detail, prefix+"semantic_freshness.pending")
			if !known || !pendingKnown || embedded != count || pending != 0 {
				return false
			}
		}
		return true
	}).Semantic.Detail
}

func TestParallelIngestionBuildsHistoryAndResumesBothStores(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	initialConfig := strings.ReplaceAll(parallelProofUnavailableConfig, "backends = [\"milvus\", \"local\"]\n", "")
	initialConfig = strings.ReplaceAll(initialConfig, "localhost:1", address)
	if endpoint := os.Getenv("CLYDE_TEST_EMBEDDING_BASE_URL"); endpoint != "" {
		initialConfig = strings.Replace(initialConfig, "sync_interval", fmt.Sprintf("embedding_base_url = %q\nsync_interval", endpoint), 1)
	}
	proof := startIngestionProofDaemonWithConfig(t, initialConfig)
	waitForParallelProofMatch(t, conversation.SearchBackendMilvus, ingestionProofMainSession, ingestionProofFirstToken)
	proof.install(t, ingestionProofSentinelSession, ingestionProofSentinelNumber, []ingestionProofMessage{
		{role: "user", body: ingestionProofSentinelText},
	}, ingestionProofFirstAge)
	waitForParallelProofMatch(t, conversation.SearchBackendMilvus, ingestionProofSentinelSession, ingestionProofSentinelToken)
	waitForIngestionProofFreshness(t, "legacy-complete", ingestionProofFailureTimeout, ingestionProofSettled(2))
	proof.stop()
	legacyDirectory := filepath.Join(daemon.ConversationCheckpointDir(), vectorsearch.CollectionName(statusProofDefaultCollectionID))
	legacy := checkpointtest.Records(t, legacyDirectory)
	if len(legacy) != 2 {
		t.Fatalf("legacy checkpoints = %v", legacy)
	}
	if err := os.Remove(filepath.Join(proof.projectDir, ingestionProofSentinelSession+".jsonl")); err != nil {
		t.Fatal(err)
	}
	parallelConfig := strings.Replace(initialConfig, "backend = \"milvus\"", "backend = \"milvus\"\nbackends = [\"milvus\", \"local\"]", 1)
	configPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "clyde", "config.toml")
	if err := os.WriteFile(configPath, []byte(parallelConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	proof.stop = runIngestionProofDaemon(t)
	metrics := waitForParallelProofComplete(t, 1)
	if statusProofInt(t, "existing-milvus", metrics, "semantic_backends.milvus.semantic_sync.upsert_totals.rows_written") != 0 ||
		statusProofInt(t, "existing-milvus", metrics, "semantic_backends.milvus.semantic_sync.upsert_totals.vectors_embedded") != 0 {
		t.Fatalf("parallel startup rewrote existing Milvus content: %+v", metrics)
	}
	waitForParallelProofMatch(t, conversation.SearchBackendLocal, ingestionProofMainSession, ingestionProofFirstToken)
	waitForParallelProofMatch(t, conversation.SearchBackendMilvus, ingestionProofSentinelSession, ingestionProofSentinelToken)
	localResult, err := daemon.SearchConversations(t.Context(), conversation.SearchConversationsOptions{
		Backend: conversation.SearchBackendLocal, Query: ingestionProofSentinelToken, Limit: ingestionProofSearchLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ingestionProofHasMatch(localResult, ingestionProofSentinelSession, ingestionProofSentinelToken) {
		t.Fatal("local search returned the conversation without an available source transcript")
	}
	defaultResult := waitForParallelProofMatch(t, "", ingestionProofMainSession, ingestionProofFirstToken)
	if defaultResult.Source != conversation.SearchSourceSemantic {
		t.Fatalf("default source = %s", defaultResult.Source)
	}
	settings := config.NewConfigWithDefaults().Conversation.Semantic
	settings.MilvusAddress = address
	stored, err := daemon.OpenConversationSearchClient(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := stored.Close(t.Context()); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	rows, err := stored.CountConversationRows(t.Context(), settings.CollectionID, "claude:"+ingestionProofSentinelSession)
	if err != nil || rows != 1 {
		t.Fatalf("retained Milvus rows=%d error=%v", rows, err)
	}
	proof.install(t, ingestionProofMainSession, ingestionProofMainNumber, []ingestionProofMessage{
		{role: "user", body: ingestionProofFirstText},
		{role: "assistant", body: ingestionProofReplyText},
		{role: "user", body: ingestionProofSecondText},
	}, ingestionProofFirstAge)
	for _, backend := range []conversation.SearchBackend{conversation.SearchBackendMilvus, conversation.SearchBackendLocal} {
		waitForParallelProofMatch(t, backend, ingestionProofMainSession, ingestionProofSecondToken)
	}
	waitForParallelProofComplete(t, 1)
	if after := checkpointtest.Records(t, legacyDirectory); !maps.Equal(legacy, after) {
		t.Fatalf("legacy checkpoints changed: before=%v after=%v", legacy, after)
	}
	proof.stop()
	proof.stop = runIngestionProofDaemon(t)
	metrics = waitForParallelProofComplete(t, 1)
	for _, backend := range []conversation.SearchBackend{conversation.SearchBackendMilvus, conversation.SearchBackendLocal} {
		waitForParallelProofMatch(t, backend, ingestionProofMainSession, ingestionProofSecondToken)
		prefix := "semantic_backends." + string(backend) + ".semantic_sync."
		if sent := statusProofInt(t, "restart", metrics, prefix+"totals.sent_conversations"); sent != 0 {
			t.Fatalf("backend=%s resent %d unchanged conversations", backend, sent)
		}
		if checkpoint := statusProofInt(t, "restart", metrics, prefix+"checkpoint.count"); checkpoint != 1 {
			t.Fatalf("backend=%s checkpoint count=%d", backend, checkpoint)
		}
	}
}
