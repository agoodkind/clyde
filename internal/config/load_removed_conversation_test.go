package config_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func writeGlobalConfig(t *testing.T, contents string) {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	globalDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

type removedKeyWarning struct {
	Level   string `json:"level"`
	Message string `json:"msg"`
	Key     string `json:"key"`
	Path    string `json:"path"`
}

func removedKeyWarnings(t *testing.T, logs *bytes.Buffer) []removedKeyWarning {
	t.Helper()
	var warnings []removedKeyWarning
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record removedKeyWarning
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}
		if record.Message == "removed configuration key" {
			warnings = append(warnings, record)
		}
	}
	return warnings
}

func TestLoadGlobalWarnsAndIgnoresRemovedConversationSemanticKeys(t *testing.T) {
	const baseConfig = "[conversation.semantic]\nsearch_enabled = true\ncollection_id = \"custom-conversations\"\n"
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for _, key := range []string{
		"socket_path", "indexed_providers", "indexed_roles", "include_archived", "include_subagents",
		"catalog_path", "lock_path", "pool_id", "milvus_collection", "embedding_max_attempts",
		"embedding_backoff_base", "embedding_revision", "vector_dimension", "normalization",
		"analyzer_identity", "max_batch_rows", "max_batch_bytes", "raw_batch_target_bytes",
		"query_block_size", "query_workers", "max_temporary_bytes", "max_snapshot_bytes", "snapshot_ttl",
		"query_timeout", "max_page_size", "max_query_bytes", "max_filter_depth", "max_filter_values",
		"bm25_k1", "bm25_b", "rrf_k",
	} {
		t.Run(key, func(t *testing.T) {
			var logs bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			writeGlobalConfig(t, baseConfig)
			want, err := config.LoadGlobalOrDefault()
			if err != nil {
				t.Fatal(err)
			}
			if warnings := removedKeyWarnings(t, &logs); len(warnings) != 0 {
				t.Fatalf("config without removed keys logged %+v", warnings)
			}
			writeGlobalConfig(t, baseConfig+key+" = 1\n")
			got, err := config.LoadGlobalOrDefault()
			if err != nil {
				t.Fatalf("load with removed key %s: %v", key, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("config with removed key %s = %+v, want %+v", key, got, want)
			}
			configPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "clyde", "config.toml")
			warnings := removedKeyWarnings(t, &logs)
			if len(warnings) != 1 || warnings[0].Level != "WARN" || warnings[0].Key != "conversation.semantic."+key || warnings[0].Path != configPath {
				t.Fatalf("removed key warnings = %+v, want one for %s at %s", warnings, key, configPath)
			}
		})
	}
}

func TestLoadGlobalRejectsEmbeddedConversationBackend(t *testing.T) {
	writeGlobalConfig(t, "[conversation.semantic]\nbackend = \"embedded\"\n")
	_, err := config.LoadGlobalOrDefault()
	if err == nil || !strings.Contains(err.Error(), "conversation.semantic.backend") {
		t.Fatalf("load error = %v, want a conversation.semantic.backend error", err)
	}
}

func TestLoadGlobalAcceptsLocalAndMilvusConversationBackends(t *testing.T) {
	writeGlobalConfig(t, "[conversation.semantic]\nbackend = \"local\"\nsearch_enabled = true\n")
	local, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	if local.Conversation.Semantic.Backend != config.ConversationSemanticBackendLocal || local.Conversation.Semantic.LocalRoot == "" || local.Conversation.Semantic.MilvusAddress != "" {
		t.Fatalf("local backend config = %+v", local.Conversation.Semantic)
	}

	writeGlobalConfig(t, "[conversation.semantic]\nbackend = \"milvus\"\nmilvus_address = \"localhost:19999\"\nmilvus_database = \"clyde_test\"\nembedding_model = \"test-model\"\nembedding_dimension = 8\n")
	milvus, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatal(err)
	}
	semantic := milvus.Conversation.Semantic
	if semantic.Backend != config.ConversationSemanticBackendMilvus || semantic.MilvusAddress != "localhost:19999" || semantic.MilvusDatabase != "clyde_test" || semantic.EmbeddingModel != "test-model" || semantic.EmbeddingDimension != 8 {
		t.Fatalf("milvus backend config = %+v", semantic)
	}
}
