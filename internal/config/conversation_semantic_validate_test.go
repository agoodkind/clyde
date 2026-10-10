package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadConversationSemanticTestConfig(t *testing.T, contents string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return loadConfig(dir)
}

// TestConversationSemanticProductionConfigStillLoads loads the key set that
// production uses today and confirms that the loader accepts it and selects
// the lm-semantic-search daemon.
func TestConversationSemanticProductionConfigStillLoads(t *testing.T) {
	t.Parallel()

	cfg, err := loadConversationSemanticTestConfig(t, `[conversation.semantic]
ingestion_enabled = true
search_enabled = true
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
		{name: "two credential references", body: "embedding_api_key_env = \"OPENAI_API_KEY\"\nembedding_api_key_file = \"/tmp/key\"\n", wantKey: "conversation.semantic.embedding_api_key_env"},
		{name: "sync_interval_below_minimum", body: "sync_interval = \"500ms\"\n", wantKey: "conversation.semantic.sync_interval"},
		{name: "sync_interval_negative", body: "sync_interval = \"-1s\"\n", wantKey: "conversation.semantic.sync_interval"},
		{name: "index_refresh_interval_below_minimum", body: "index_refresh_interval = \"999ms\"\n", wantKey: "conversation.semantic.index_refresh_interval"},
		{name: "index_refresh_interval_negative", body: "index_refresh_interval = \"-1s\"\n", wantKey: "conversation.semantic.index_refresh_interval"},
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

func TestConversationSemanticWorkerIntervals(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name             string
		body             string
		wantSync         time.Duration
		wantIndexRefresh time.Duration
	}{
		{name: "unset", body: "", wantSync: time.Minute, wantIndexRefresh: time.Minute},
		{name: "explicit", body: "sync_interval = \"2s\"\nindex_refresh_interval = \"5s\"\n", wantSync: 2 * time.Second, wantIndexRefresh: 5 * time.Second},
		{name: "minimum", body: "sync_interval = \"1s\"\nindex_refresh_interval = \"1s\"\n", wantSync: time.Second, wantIndexRefresh: time.Second},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := loadConversationSemanticTestConfig(t, "[conversation.semantic]\n"+testCase.body)
			if err != nil {
				t.Fatalf("operation=load_config err=%v", err)
			}
			semantic := cfg.Conversation.Semantic
			if got := semantic.SyncPassInterval(); got != testCase.wantSync {
				t.Fatalf("key=sync_interval got=%s want=%s", got, testCase.wantSync)
			}
			if got := semantic.IndexRefreshPeriod(); got != testCase.wantIndexRefresh {
				t.Fatalf("key=index_refresh_interval got=%s want=%s", got, testCase.wantIndexRefresh)
			}
		})
	}
}

// TestConversationSemanticSettingChangeRoutesToReload changes one key
// and requires the change classifier to choose a reload.
func TestConversationSemanticSettingChangeRoutesToReload(t *testing.T) {
	t.Parallel()

	oldCfg := *NewConfig()
	newCfg := *NewConfig()
	newCfg.Conversation.Semantic.EmbeddingModel = "other-model"
	if route := ClassifyConfigChange(oldCfg, newCfg); route != RouteReload {
		t.Fatalf("route = %s, want reload", route)
	}
}
