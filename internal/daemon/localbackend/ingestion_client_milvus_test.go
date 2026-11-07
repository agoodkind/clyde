//go:build milvusintegration

package localbackend_test

import (
	"os"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

func TestIndependentIngestionBackendsKeepTheirOwnProgress(t *testing.T) {
	milvustest.RequireEnabled(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	settings := config.NewConfigWithDefaults().Conversation.Semantic
	settings.MilvusAddress = milvustest.Start(t)
	if endpoint := os.Getenv("CLYDE_TEST_EMBEDDING_BASE_URL"); endpoint != "" {
		settings.EmbeddingBaseURL = endpoint
	}
	localSettings := settings.ForSearchBackend(config.ConversationSemanticBackendLocal)
	localSettings.LocalRoot = t.TempDir()
	local, closeLocal := openIngestionCheckpointClient(t, localSettings)
	milvus, closeMilvus := openIngestionCheckpointClient(t, settings)
	requireIngestionCheckpointNeeded(t, local, ingestionCheckpointFirst, ingestionCheckpointSecond)
	requireIngestionCheckpointNeeded(t, milvus, ingestionCheckpointFirst, ingestionCheckpointSecond)
	writeIngestionCheckpointDocument(t, local, 0)
	writeIngestionCheckpointDocument(t, milvus, 1)
	requireIngestionCheckpointNeeded(t, local, ingestionCheckpointSecond)
	requireIngestionCheckpointNeeded(t, milvus, ingestionCheckpointFirst)
	closeLocal()
	closeMilvus()

	localRestart, closeLocalRestart := openIngestionCheckpointClient(t, localSettings)
	settings.Backend = config.ConversationSemanticBackendLMS
	milvusRestart, closeMilvusRestart := openIngestionCheckpointClient(t, settings)
	requireIngestionCheckpointNeeded(t, localRestart, ingestionCheckpointSecond)
	requireIngestionCheckpointNeeded(t, milvusRestart, ingestionCheckpointFirst)
	writeIngestionCheckpointDocument(t, localRestart, 1)
	requireIngestionCheckpointNeeded(t, localRestart)
	requireIngestionCheckpointNeeded(t, milvusRestart, ingestionCheckpointFirst)
	writeIngestionCheckpointDocument(t, milvusRestart, 0)
	requireIngestionCheckpointNeeded(t, milvusRestart)
	closeLocalRestart()
	closeMilvusRestart()

	completedLocal, _ := openIngestionCheckpointClient(t, localSettings)
	settings.Backend = config.ConversationSemanticBackendMilvus
	completedMilvus, _ := openIngestionCheckpointClient(t, settings)
	requireIngestionCheckpointNeeded(t, completedLocal)
	requireIngestionCheckpointNeeded(t, completedMilvus)
}
