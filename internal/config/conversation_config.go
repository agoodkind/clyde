package config

import "strings"

const defaultConversationSemanticCollectionID = "clyde-conversations"

// ConversationConfig configures raw conversation indexing integrations.
type ConversationConfig struct {
	// IncludeSubagentConversations exposes the transcripts a dispatched agent
	// wrote alongside the conversations a person started. It defaults to false
	// because one working session that dispatches agents can write hundreds of
	// agent transcripts in a day, which crowd list and search results and consume
	// embedding capacity ahead of the user's own work. Turning it on takes effect
	// on the next daemon generation and needs no cache deletion, because the index
	// stores every conversation's origin and applies this setting when it reads.
	IncludeSubagentConversations bool                       `json:"includeSubagentConversations,omitempty" toml:"include_subagent_conversations,omitempty"`
	Semantic                     ConversationSemanticConfig `json:"semantic,omitzero" toml:"semantic,omitempty"`
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
