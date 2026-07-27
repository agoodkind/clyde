package daemon

import (
	"goodkind.io/clyde/internal/config"
)

// SemanticContentPolicy declares which content classes the feeder offers to the
// search engine. It is the resolved form of
// `conversation.semantic.indexed_content`: the config carries a list because an
// operator writes names, and this carries one field per class because the
// document builder asks about one class at a time.
//
// It selects parts of a message. The conversation itself is still delivered, so
// a class turned off here is reconciled away by the engine's normal message diff
// rather than retained. That is the opposite of
// `conversation.include_subagent_conversations`, which hides whole conversations
// and therefore shortens the manifest, which the engine retains.
type SemanticContentPolicy struct {
	Text           bool
	Reasoning      bool
	ToolCall       bool
	ToolCommand    bool
	ToolInput      bool
	ToolOutput     bool
	SystemMessages bool
}

// NewSemanticContentPolicy resolves the configured class list into the policy
// the document builder reads.
//
// An empty list resolves to the default class set rather than to a policy that
// offers nothing. The config loader already fills the default, so this repeats
// it on purpose: a Config built as a struct literal never passes through the
// loader, and reading its unset list as "index nothing" would quietly empty the
// corpus instead of failing. Turning indexing off is `enabled = false`.
func NewSemanticContentPolicy(classes []config.IndexedContentClass) SemanticContentPolicy {
	if len(classes) == 0 {
		classes = config.DefaultIndexedContent()
	}
	policy := SemanticContentPolicy{
		Text:           false,
		Reasoning:      false,
		ToolCall:       false,
		ToolCommand:    false,
		ToolInput:      false,
		ToolOutput:     false,
		SystemMessages: false,
	}
	for _, class := range classes {
		switch class {
		case config.IndexedContentText:
			policy.Text = true
		case config.IndexedContentReasoning:
			policy.Reasoning = true
		case config.IndexedContentToolCall:
			policy.ToolCall = true
		case config.IndexedContentToolCommand:
			policy.ToolCommand = true
		case config.IndexedContentToolInput:
			policy.ToolInput = true
		case config.IndexedContentToolOutput:
			policy.ToolOutput = true
		case config.IndexedContentSystemMessages:
			policy.SystemMessages = true
		}
	}
	return policy
}

// SemanticContentPolicyFromConfig resolves the policy the daemon and the backfill
// command both run under, so the two never disagree about what belongs in the
// index. A backfill that indexed a class the feeder withholds would re-add rows
// the next sync removes.
func SemanticContentPolicyFromConfig(semantic config.ConversationSemanticConfig) SemanticContentPolicy {
	return NewSemanticContentPolicy(semantic.IndexedContent)
}
