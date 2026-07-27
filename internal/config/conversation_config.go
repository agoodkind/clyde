package config

import (
	"fmt"
	"slices"
	"strings"
)

const defaultConversationSemanticCollectionID = "clyde-conversations"

// IndexedContentClass names one kind of content a conversation message carries.
// It is the closed enum for `conversation.semantic.indexed_content`.
//
// The classes match the chunk families the search engine stores, so naming one
// here governs exactly one family of embedded rows rather than a vague notion of
// relevance. A class the list omits is still read from the provider artifact and
// still rendered by export; only the embedding index skips it.
type IndexedContentClass string

const (
	// IndexedContentText is the message text a person or a model wrote.
	IndexedContentText IndexedContentClass = "text"
	// IndexedContentReasoning is a model's private reasoning block. It is absent
	// from the default set, because reasoning is working-out rather than
	// conclusion, so embedding it by default puts a model's intermediate thinking
	// into the results of someone searching their own conversation.
	IndexedContentReasoning IndexedContentClass = "reasoning"
	// IndexedContentToolCall is the tool's name and call token.
	IndexedContentToolCall IndexedContentClass = "tool_call"
	// IndexedContentToolCommand is the shell command a tool call carried.
	IndexedContentToolCommand IndexedContentClass = "tool_command"
	// IndexedContentToolInput is the tool call's JSON arguments.
	IndexedContentToolInput IndexedContentClass = "tool_input"
	// IndexedContentToolOutput is what a tool returned.
	IndexedContentToolOutput IndexedContentClass = "tool_output"
	// IndexedContentSystemMessages is the provider's own system and control
	// messages. It is absent from the default set, and the transcript loader has
	// excluded them all along, so naming it here is what makes them reachable
	// rather than what hides them.
	IndexedContentSystemMessages IndexedContentClass = "system_messages"
)

// DefaultIndexedContent is the content offered to the engine when the config
// names no classes. Reasoning and system messages are absent, so a fresh install
// embeds what a person would search for and nothing else.
//
// It is exported because the policy resolver repeats this default for a Config
// built as a struct literal, which never passes through the loader.
func DefaultIndexedContent() []IndexedContentClass {
	return []IndexedContentClass{
		IndexedContentText,
		IndexedContentToolCall,
		IndexedContentToolCommand,
		IndexedContentToolInput,
		IndexedContentToolOutput,
	}
}

// knownIndexedContent lists every accepted class, so an unrecognized name in the
// config fails the load instead of silently indexing less than the operator
// asked for.
func knownIndexedContent() []IndexedContentClass {
	return []IndexedContentClass{
		IndexedContentText,
		IndexedContentReasoning,
		IndexedContentToolCall,
		IndexedContentToolCommand,
		IndexedContentToolInput,
		IndexedContentToolOutput,
		IndexedContentSystemMessages,
	}
}

// Valid reports whether the class is one this build understands.
func (c IndexedContentClass) Valid() bool {
	return slices.Contains(knownIndexedContent(), c)
}

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
	// IndexedContent names the content classes offered to the search engine. It
	// selects parts of a message, which is a different level from
	// [ConversationConfig.IncludeSubagentConversations]. The conversation is still
	// delivered, so the engine reconciles the message rows it stops receiving
	// rather than retaining them.
	//
	// An absent or empty list means the default set. Naming no classes is not how
	// an operator turns indexing off, because that would quietly stop embedding
	// everything; `enabled = false` is.
	IndexedContent []IndexedContentClass `json:"indexedContent,omitempty" toml:"indexed_content,omitempty"`
}

// applyConversationDefaults fills the conversation defaults and rejects an
// unrecognized content class, so a typo fails the load rather than silently
// narrowing what is indexed.
func applyConversationDefaults(conversation *ConversationConfig) error {
	if conversation == nil {
		return nil
	}
	conversation.Semantic.SocketPath = strings.TrimSpace(conversation.Semantic.SocketPath)
	conversation.Semantic.CollectionID = strings.TrimSpace(conversation.Semantic.CollectionID)
	if conversation.Semantic.CollectionID == "" {
		conversation.Semantic.CollectionID = defaultConversationSemanticCollectionID
	}
	normalized, err := normalizeIndexedContent(conversation.Semantic.IndexedContent)
	if err != nil {
		return err
	}
	conversation.Semantic.IndexedContent = normalized
	return nil
}

func normalizeIndexedContent(classes []IndexedContentClass) ([]IndexedContentClass, error) {
	normalized := make([]IndexedContentClass, 0, len(classes))
	seen := make(map[IndexedContentClass]bool, len(classes))
	for _, class := range classes {
		trimmed := IndexedContentClass(strings.ToLower(strings.TrimSpace(string(class))))
		if trimmed == "" {
			continue
		}
		if !trimmed.Valid() {
			return nil, fmt.Errorf(
				"conversation.semantic.indexed_content contains unknown class %q; supported classes are %s",
				string(class),
				indexedContentClassNames(),
			)
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		normalized = append(normalized, trimmed)
	}
	if len(normalized) == 0 {
		return DefaultIndexedContent(), nil
	}
	return normalized, nil
}

func indexedContentClassNames() string {
	known := knownIndexedContent()
	names := make([]string, 0, len(known))
	for _, class := range known {
		names = append(names, string(class))
	}
	return strings.Join(names, "|")
}
