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
	//
	// It selects whole conversations, which is a different level from
	// [ConversationSemanticConfig.IndexedContent]. A conversation this hides is
	// absent from every clyde surface, and the engine retains whatever a short
	// manifest omits, so hiding one removes nothing already stored.
	IncludeSubagentConversations bool                       `json:"includeSubagentConversations,omitempty" toml:"include_subagent_conversations,omitempty"`
	Semantic                     ConversationSemanticConfig `json:"semantic,omitzero" toml:"semantic,omitempty"`
}

// ConversationSemanticConfig configures background semantic-search sync.
type ConversationSemanticConfig struct {
	Enabled      bool   `json:"enabled,omitempty" toml:"enabled,omitempty"`
	SocketPath   string `json:"socketPath,omitempty" toml:"socket_path,omitempty"`
	CollectionID string `json:"collectionId,omitempty" toml:"collection_id,omitempty"`
	// IndexedContent names the content kinds offered to the search engine, using
	// the same selector vocabulary the export surface accepts. The names and their
	// validation belong to the conversation package's content-kind taxonomy, which
	// this package cannot import without a cycle, so the values are carried as
	// written and resolved where they are used.
	//
	// It selects parts of a message, which is a different level from
	// [ConversationConfig.IncludeSubagentConversations]. The conversation is still
	// delivered, so the engine reconciles the message rows it stops receiving
	// rather than retaining them.
	//
	// An absent or empty list means the indexing default. Naming no kinds is not
	// how an operator turns indexing off, because that would quietly stop
	// embedding everything; `enabled = false` is.
	IndexedContent []string `json:"indexedContent,omitempty" toml:"indexed_content,omitempty"`
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
	trimmed := make([]string, 0, len(conversation.Semantic.IndexedContent))
	for _, value := range conversation.Semantic.IndexedContent {
		if selector := strings.TrimSpace(value); selector != "" {
			trimmed = append(trimmed, selector)
		}
	}
	conversation.Semantic.IndexedContent = trimmed
}
