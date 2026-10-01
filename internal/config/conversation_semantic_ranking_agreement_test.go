package config

import (
	"testing"

	"goodkind.io/lm-semantic-search/library"
)

// TestConversationSemanticRankingMatchesLibraryValidation loads each ranking
// value through the Clyde config loader and passes the same value to
// library.Config.Validate. The loader must reject exactly the values the
// pinned library rejects at Open.
func TestConversationSemanticRankingMatchesLibraryValidation(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		toml   string
		apply  func(*library.Config)
		accept bool
	}{
		{name: "k1 zero", toml: "bm25_k1 = 0.0", apply: func(c *library.Config) { c.BM25K1 = 0 }, accept: true},
		{name: "k1 default", toml: "bm25_k1 = 1.2", apply: func(c *library.Config) { c.BM25K1 = 1.2 }, accept: true},
		{name: "k1 at bound", toml: "bm25_k1 = 1e6", apply: func(c *library.Config) { c.BM25K1 = 1e6 }, accept: true},
		{name: "k1 above bound", toml: "bm25_k1 = 1000000.5", apply: func(c *library.Config) { c.BM25K1 = 1000000.5 }, accept: false},
		{name: "k1 float32 underflow", toml: "bm25_k1 = 1e-50", apply: func(c *library.Config) { c.BM25K1 = 1e-50 }, accept: false},
		{name: "k1 negative", toml: "bm25_k1 = -0.5", apply: func(c *library.Config) { c.BM25K1 = -0.5 }, accept: false},
		{name: "b zero", toml: "bm25_b = 0.0", apply: func(c *library.Config) { value := 0.0; c.BM25B = &value }, accept: true},
		{name: "b above one", toml: "bm25_b = 1.5", apply: func(c *library.Config) { value := 1.5; c.BM25B = &value }, accept: false},
		{name: "rrf_k negative", toml: "rrf_k = -1", apply: func(c *library.Config) { c.RRFK = -1 }, accept: false},
		{name: "query block at bound", toml: "query_block_size = 16384", apply: func(c *library.Config) { c.QueryBlockSize = 16384 }, accept: true},
		{name: "query block above bound", toml: "query_block_size = 16385", apply: func(c *library.Config) { c.QueryBlockSize = 16385 }, accept: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, loadErr := loadConversationSemanticTestConfig(t, "[conversation.semantic]\n"+testCase.toml+"\n")
			libraryConfig := library.Config{
				Store: library.StoreDescriptor{
					CatalogPath:       "/tmp/clyde-test/catalog.sqlite",
					LockPath:          "/tmp/clyde-test/catalog.lock",
					PoolID:            "conversations-v1",
					EmbeddingModel:    "nvidia/NV-EmbedCode-7b-v1",
					EmbeddingRevision: "r1",
					Dimension:         4096,
					Normalization:     "l2",
				},
			}
			testCase.apply(&libraryConfig)
			libraryErr := libraryConfig.Validate()

			if (libraryErr == nil) != testCase.accept {
				t.Fatalf("library.Config.Validate error = %v, want accept=%t", libraryErr, testCase.accept)
			}
			if (loadErr == nil) != testCase.accept {
				t.Fatalf("Clyde config load error = %v, want accept=%t to match the library", loadErr, testCase.accept)
			}
		})
	}
}
