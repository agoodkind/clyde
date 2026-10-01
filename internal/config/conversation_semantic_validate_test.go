package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConversationSemanticTestConfig(t *testing.T, contents string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return loadConfig(dir)
}

// embeddedSemanticSettings is a complete embedded backend section body without
// the backend key.
const embeddedSemanticSettings = `catalog_path = "/tmp/clyde-test/catalog.sqlite"
lock_path = "/tmp/clyde-test/catalog.lock"
pool_id = "conversations-v1"
milvus_address = "localhost:19530"
milvus_database = "clyde_test"
milvus_collection = "conversation_vectors"
embedding_base_url = "http://localhost:5400/v1"
embedding_model = "nvidia/NV-EmbedCode-7b-v1"
embedding_revision = "r1"
vector_dimension = 4096
normalization = "l2"
`

// TestConversationSemanticProductionConfigStillLoads loads the key set that
// production uses today and confirms that the loader accepts it and selects
// the lm-semantic-search daemon.
func TestConversationSemanticProductionConfigStillLoads(t *testing.T) {
	t.Parallel()

	cfg, err := loadConversationSemanticTestConfig(t, `[conversation.semantic]
ingestion_enabled = true
search_enabled = true
socket_path = "/tmp/lms.sock"
collection_id = "clyde-conversations"
`)
	if err != nil {
		t.Fatalf("load production-shaped config: %v", err)
	}
	semantic := cfg.Conversation.Semantic
	if semantic.Backend != "" {
		t.Fatalf("backend = %q, want empty for the lm-semantic-search daemon", semantic.Backend)
	}
	if !semantic.FeedsEngine() || !semantic.AnswersSearch() {
		t.Fatalf("semantic directions = feeds %v answers %v, want both true", semantic.FeedsEngine(), semantic.AnswersSearch())
	}
}

// TestConversationSemanticEmbeddedSettingsParse loads every embedded key under
// the lms backend and checks the typed values, including an explicit bm25_b
// of zero.
func TestConversationSemanticEmbeddedSettingsParse(t *testing.T) {
	t.Parallel()

	cfg, err := loadConversationSemanticTestConfig(t, "[conversation.semantic]\nbackend = \"lms\"\n"+embeddedSemanticSettings+`indexed_providers = [" claude ", ""]
indexed_roles = ["user"]
include_archived = true
include_subagents = true
embedding_api_key_env = "OPENAI_API_KEY"
embedding_request_timeout = "20s"
embedding_max_attempts = 5
embedding_backoff_base = "300ms"
query_instruction_prefix = "query: "
max_batch_rows = 128
max_batch_bytes = 4194304
raw_batch_target_bytes = 4194304
query_block_size = 256
query_workers = 4
max_temporary_bytes = 1073741824
max_snapshot_bytes = 268435456
snapshot_ttl = "5m"
query_timeout = "15s"
max_page_size = 100
max_query_bytes = 8192
max_filter_depth = 8
max_filter_values = 4096
bm25_k1 = 1.4
bm25_b = 0.0
rrf_k = 40
`)
	if err != nil {
		t.Fatalf("load embedded settings under the lms backend: %v", err)
	}
	semantic := cfg.Conversation.Semantic
	if semantic.Backend != ConversationSemanticBackendLMS {
		t.Fatalf("backend = %q, want lms", semantic.Backend)
	}
	if len(semantic.IndexedProviders) != 1 || semantic.IndexedProviders[0] != "claude" {
		t.Fatalf("indexed providers = %q, want [claude]", semantic.IndexedProviders)
	}
	if semantic.EmbeddingMaxAttempts == nil || *semantic.EmbeddingMaxAttempts != 5 {
		t.Fatalf("embedding max attempts = %v, want 5", semantic.EmbeddingMaxAttempts)
	}
	if semantic.QueryTimeout.AsDuration().String() != "15s" {
		t.Fatalf("query timeout = %v, want 15s", semantic.QueryTimeout.AsDuration())
	}
	if semantic.BM25B == nil || *semantic.BM25B != 0 {
		t.Fatalf("bm25_b = %v, want an explicit zero", semantic.BM25B)
	}
	if semantic.VectorDimension != 4096 || semantic.RRFK != 40 {
		t.Fatalf("dimension and rrf_k = %d and %d, want 4096 and 40", semantic.VectorDimension, semantic.RRFK)
	}
}

// TestConversationSemanticRejectsInvalidSettings loads one invalid setting per
// case and requires an error that contains the offending key.
func TestConversationSemanticRejectsInvalidSettings(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name    string
		body    string
		wantKey string
	}{
		{name: "unknown backend", body: "backend = \"remote\"\n", wantKey: "conversation.semantic.backend"},
		{name: "negative timeout", body: "query_timeout = \"-1s\"\n", wantKey: "conversation.semantic.query_timeout"},
		{name: "negative budget", body: "max_snapshot_bytes = -1\n", wantKey: "conversation.semantic.max_snapshot_bytes"},
		{name: "zero attempts", body: "embedding_max_attempts = 0\n", wantKey: "conversation.semantic.embedding_max_attempts"},
		{name: "two credential references", body: "embedding_api_key_env = \"OPENAI_API_KEY\"\nembedding_api_key_file = \"/tmp/key\"\n", wantKey: "conversation.semantic.embedding_api_key_env"},
		{name: "bm25_b above one", body: "bm25_b = 1.5\n", wantKey: "conversation.semantic.bm25_b"},
		{name: "negative bm25_k1", body: "bm25_k1 = -0.1\n", wantKey: "conversation.semantic.bm25_k1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadConversationSemanticTestConfig(t, "[conversation.semantic]\n"+testCase.body)
			if err == nil {
				t.Fatalf("load succeeded, want an error for %s", testCase.wantKey)
			}
			if !strings.Contains(err.Error(), testCase.wantKey) {
				t.Fatalf("error = %q, want it to contain %s", err.Error(), testCase.wantKey)
			}
		})
	}
}

// TestConversationSemanticEmbeddedBackendChecksRequiredSettings selects the
// embedded backend four times. The first load omits pool_id and fails on that
// key. The second load sets a relative catalog_path and fails on the absolute
// path requirement. The third load sets a complete ingestion-only section and
// succeeds. The fourth load adds search_enabled = true and fails because this
// build has no embedded search.
func TestConversationSemanticEmbeddedBackendChecksRequiredSettings(t *testing.T) {
	t.Parallel()

	withoutPool := strings.Replace(embeddedSemanticSettings, "pool_id = \"conversations-v1\"\n", "", 1)
	_, err := loadConversationSemanticTestConfig(t, "[conversation.semantic]\nbackend = \"embedded\"\n"+withoutPool)
	if err == nil || !strings.Contains(err.Error(), "conversation.semantic.pool_id is required") {
		t.Fatalf("missing pool_id error = %v, want the pool_id requirement", err)
	}

	relativeCatalog := strings.Replace(embeddedSemanticSettings, "/tmp/clyde-test/catalog.sqlite", "catalog.sqlite", 1)
	_, err = loadConversationSemanticTestConfig(t, "[conversation.semantic]\nbackend = \"embedded\"\n"+relativeCatalog)
	if err == nil || !strings.Contains(err.Error(), "conversation.semantic.catalog_path must be an absolute path") {
		t.Fatalf("relative catalog_path error = %v, want the absolute path requirement", err)
	}

	cfg, err := loadConversationSemanticTestConfig(t, "[conversation.semantic]\nbackend = \"embedded\"\ningestion_enabled = true\n"+embeddedSemanticSettings)
	if err != nil {
		t.Fatalf("load complete ingestion-only embedded section: %v", err)
	}
	semantic := cfg.Conversation.Semantic
	if semantic.Backend != ConversationSemanticBackendEmbedded || !semantic.FeedsEngine() || semantic.AnswersSearch() {
		t.Fatalf("backend/feeds/answers = %q/%v/%v, want embedded/true/false", semantic.Backend, semantic.FeedsEngine(), semantic.AnswersSearch())
	}

	_, err = loadConversationSemanticTestConfig(t, "[conversation.semantic]\nbackend = \"embedded\"\nsearch_enabled = true\n"+embeddedSemanticSettings)
	if err == nil || !strings.Contains(err.Error(), "embedded search is not available in this Clyde build (CLYDE-761)") {
		t.Fatalf("embedded section with search enabled error = %v, want the unavailable search error", err)
	}
}

// TestConversationSemanticSettingChangeRoutesToReload changes one embedded key
// and requires the change classifier to choose a reload.
func TestConversationSemanticSettingChangeRoutesToReload(t *testing.T) {
	t.Parallel()

	oldCfg := *NewConfig()
	newCfg := *NewConfig()
	newCfg.Conversation.Semantic.QueryWorkers = 4
	if route := ClassifyConfigChange(oldCfg, newCfg); route != RouteReload {
		t.Fatalf("route = %s, want reload", route)
	}
}
