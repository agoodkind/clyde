package config

import "testing"

func TestMilvusEmbeddingIdlePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		toml    string
		enabled bool
	}{
		{name: "default", toml: "", enabled: false},
		{name: "Milvus alone", toml: "milvus_embedding_idle_only = true", enabled: true},
		{
			name:    "Milvus and local",
			toml:    "backends = [\"milvus\", \"local\"]\nmilvus_embedding_idle_only = true",
			enabled: true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name,
			func(t *testing.T) {
				cfg, err := loadConversationSemanticTestConfig(
					t,
					"[conversation.semantic]\n"+test.toml,
				)
				if err != nil {
					t.Fatal(err)
				}

				if enabled := cfg.Conversation.Semantic.MilvusEmbeddingIdleOnly; enabled != test.enabled {
					t.Fatalf("idle policy = %v, want %v", enabled, test.enabled)
				}
			},
		)
	}
}

func TestMilvusEmbeddingIdlePolicyRequiresReload(t *testing.T) {
	t.Parallel()

	base := *NewConfigWithDefaults()
	changed := base
	changed.Conversation.Semantic.MilvusEmbeddingIdleOnly = true

	if got := ClassifyConfigChange(base, changed); got != RouteReload {
		t.Fatalf("config change classified as %s, want reload", got)
	}
}
