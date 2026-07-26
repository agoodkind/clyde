package config

import "strings"

const defaultConversationSemanticCollectionID = "clyde-conversations"

// ConversationConfig configures raw conversation indexing integrations.
type ConversationConfig struct {
	Semantic ConversationSemanticConfig `json:"semantic,omitzero" toml:"semantic,omitempty"`
}

// ConversationSemanticConfig configures background semantic-search sync.
type ConversationSemanticConfig struct {
	Enabled      bool   `json:"enabled,omitempty" toml:"enabled,omitempty"`
	SocketPath   string `json:"socketPath,omitempty" toml:"socket_path,omitempty"`
	CollectionID string `json:"collectionId,omitempty" toml:"collection_id,omitempty"`
}

func applyConversationDefaults(conversation *ConversationConfig) {
	if conversation == nil {
		return
	}
	conversation.Semantic.SocketPath = strings.TrimSpace(conversation.Semantic.SocketPath)
	conversation.Semantic.CollectionID = strings.TrimSpace(conversation.Semantic.CollectionID)
	if conversation.Semantic.CollectionID == "" {
		conversation.Semantic.CollectionID = defaultConversationSemanticCollectionID
	}
}
