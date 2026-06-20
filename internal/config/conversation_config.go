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
	// LiteralFallback re-enables the literal transcript scan when the engine is
	// configured but produces no matches. It defaults to false so a cold or
	// empty engine surfaces a loud LiteralDisabledCold result instead of a
	// misleading literal scan, forcing the semantic path to carry its own
	// weight. It has no effect when the engine is not configured at all, where
	// the literal scan is the only search path.
	LiteralFallback bool `json:"literalFallback,omitempty" toml:"literal_fallback,omitempty"`
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
