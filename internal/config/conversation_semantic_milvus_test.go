package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestConversationSemanticMilvusConfig(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		mode       ConversationSemanticMilvusQueryMode
		window     int
		verifyRows int
		block      int
		wantKey    string
	}{
		{name: "defaults"},
		{name: "normal maximum", mode: "normal", block: 16384},
		{name: "normal excessive block", block: 16385, wantKey: "query_block_size"},
		{name: "normal score window", window: 32768, wantKey: "milvus_max_score_window"},
		{name: "unknown mode", mode: "other", wantKey: "milvus_query_mode"},
		{name: "large mode", mode: "large_topk", window: 32768, verifyRows: 4096, block: 32768},
		{name: "large missing window", mode: "large_topk", wantKey: "milvus_max_score_window"},
		{name: "large excessive window", mode: "large_topk", window: 1000001, wantKey: "milvus_max_score_window"},
		{name: "large excessive block", mode: "large_topk", window: 32768, block: 32769, wantKey: "query_block_size"},
		{name: "excessive verification", verifyRows: 4097, wantKey: "milvus_max_verify_batch_rows"},
		{name: "negative verification", verifyRows: -1, wantKey: "milvus_max_verify_batch_rows"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", root)
			cfg := NewConfig()
			cfg.Conversation.Semantic.MilvusQueryMode = testCase.mode
			cfg.Conversation.Semantic.MilvusMaxScoreWindow = testCase.window
			cfg.Conversation.Semantic.MilvusMaxVerifyBatchRows = testCase.verifyRows
			cfg.Conversation.Semantic.QueryBlockSize = testCase.block
			data, err := toml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := GlobalConfigPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadGlobalOrDefault()
			if testCase.wantKey != "" {
				if err == nil || !strings.Contains(err.Error(), "conversation.semantic."+testCase.wantKey) {
					t.Fatalf("load error = %v, want %s", err, testCase.wantKey)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			semantic := loaded.Conversation.Semantic
			if semantic.MilvusQueryMode != testCase.mode || semantic.MilvusMaxScoreWindow != testCase.window || semantic.MilvusMaxVerifyBatchRows != testCase.verifyRows || semantic.QueryBlockSize != testCase.block {
				t.Fatalf("loaded Milvus settings = %+v", semantic)
			}
		})
	}
}
