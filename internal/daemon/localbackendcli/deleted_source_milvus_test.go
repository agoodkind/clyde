//go:build milvusintegration

package localbackendcli_test

import (
	"fmt"
	"os"
	"testing"

	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	deletedSourceEmbeddingEnv     = "CLYDE_TEST_EMBEDDING_BASE_URL"
	deletedSourceEmbeddingDefault = "http://[::1]:5400/v1"
	deletedSourceEmbeddingModel   = "nvidia/NV-EmbedCode-7b-v1"
	deletedSourceEmbeddingDim     = 4096
	deletedSourceSettingsTemplate = `milvus_address = %q
milvus_database = "default"
embedding_base_url = %q
embedding_model = %q
embedding_dimension = %d
`
)

// TestMilvusBackendKeepsDeletedSourceSearchable checks deleted conversation matches after a rescan and restart using a throwaway Milvus stack, the real embedding endpoint, and the semantic search source.
func TestMilvusBackendKeepsDeletedSourceSearchable(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	baseURL := os.Getenv(deletedSourceEmbeddingEnv)
	if baseURL == "" {
		baseURL = deletedSourceEmbeddingDefault
	}
	settingsPath := writeSandboxSemanticSettings(t, fmt.Sprintf(deletedSourceSettingsTemplate, address, baseURL, deletedSourceEmbeddingModel, deletedSourceEmbeddingDim))
	checkDeletedSourceSearchable(t, []string{"--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath}, localProofSourceSemantic)
}
