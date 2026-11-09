package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const conversationBackendCheckpointDirName = "backends"

// OpenConversationIngestionClient opens the selected backend with independent
// checkpoints. It does not read or change the legacy shared checkpoints.
//
// The caller closes the returned client after ingestion stops.
//
//	client, err := OpenConversationIngestionClient(ctx, semanticConfig)
func OpenConversationIngestionClient(ctx context.Context, semanticCfg config.ConversationSemanticConfig) (*vectorsearch.Client, error) {
	backend := semanticCfg.Backend
	switch backend {
	case "", config.ConversationSemanticBackendLMS, config.ConversationSemanticBackendMilvus:
		backend = config.ConversationSemanticBackendMilvus
	case config.ConversationSemanticBackendLocal:
	default:
		return nil, semanticConnectorError{cause: fmt.Errorf("invalid conversation ingestion backend %q", backend)}
	}
	settings := semanticCfg.ForSearchBackend(backend)
	checkpointDir := filepath.Join(ConversationCheckpointDir(), conversationBackendCheckpointDirName, string(backend))
	return openConversationSearchClient(ctx, settings, checkpointDir)
}
