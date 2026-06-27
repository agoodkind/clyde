package zedstore

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// CurrentThreadVersion is the Zed thread JSON version Clyde currently models
// directly.
const CurrentThreadVersion = "0.3.0"

// ThreadDocument is the typed view of one Zed thread payload after any needed
// decode or legacy upgrade step.
type ThreadDocument struct {
	Version         string           `json:"version"`
	Title           string           `json:"title"`
	UpdatedAt       time.Time        `json:"updated_at"`
	DetailedSummary string           `json:"detailed_summary"`
	Model           *ThreadModel     `json:"model"`
	SubagentContext *SubagentContext `json:"subagent_context"`
	Messages        []ThreadMessage  `json:"messages"`
}

// ThreadModel is the provider and model pair Zed stored for one thread.
type ThreadModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// SubagentContext is the Zed parent-thread context persisted for a subagent
// thread.
type SubagentContext struct {
	ParentThreadID string `json:"parent_thread_id"`
	Depth          int    `json:"depth"`
}

// ThreadMessageKind identifies the Zed thread message variant after decoding.
type ThreadMessageKind string

const (
	// ThreadMessageKindUser marks a user-authored thread message.
	ThreadMessageKindUser ThreadMessageKind = "user"
	// ThreadMessageKindAgent marks an assistant-authored thread message.
	ThreadMessageKindAgent ThreadMessageKind = "agent"
	// ThreadMessageKindResume marks a resume control message.
	ThreadMessageKindResume ThreadMessageKind = "resume"
	// ThreadMessageKindCompaction marks a compaction control message.
	ThreadMessageKindCompaction ThreadMessageKind = "compaction"
)

// ThreadMessage is the decoded Zed message union flattened into explicit
// optional fields.
type ThreadMessage struct {
	Kind       ThreadMessageKind
	User       *UserMessage
	Agent      *AgentMessage
	Compaction *CompactionMessage
}

type variantKind string

const (
	variantKindUser       variantKind = "User"
	variantKindAgent      variantKind = "Agent"
	variantKindCompaction variantKind = "Compaction"
)

// UserMessage is one decoded Zed user message payload.
type UserMessage struct {
	ID      string            `json:"id"`
	Content []UserContentPart `json:"content"`
}

// UserContentKind identifies one Zed user content block variant.
type UserContentKind string

const (
	// UserContentKindText marks a plain text user content block.
	UserContentKindText UserContentKind = "Text"
	// UserContentKindMention marks a mentioned-resource user content block.
	UserContentKindMention UserContentKind = "Mention"
)

// UserContentPart is one decoded Zed user content block.
type UserContentPart struct {
	Kind    UserContentKind
	Text    string
	Mention *MentionPart
}

// MentionPart is the file, rule, or URI attachment content Zed stores inside a
// user message.
type MentionPart struct {
	URI     string `json:"uri"`
	Content string `json:"content"`
}

// AgentMessage is one decoded Zed assistant message payload.
type AgentMessage struct {
	Content          []AgentContentPart    `json:"content"`
	ToolResults      map[string]ToolResult `json:"tool_results"`
	ReasoningDetails json.RawMessage       `json:"reasoning_details"`
}

// AgentContentKind identifies one Zed assistant content block variant.
type AgentContentKind string

const (
	// AgentContentKindText marks a plain text assistant content block.
	AgentContentKindText AgentContentKind = "Text"
	// AgentContentKindThinking marks a visible thinking block.
	AgentContentKindThinking AgentContentKind = "Thinking"
	// AgentContentKindRedactedThinking marks a redacted thinking block.
	AgentContentKindRedactedThinking AgentContentKind = "RedactedThinking"
	// AgentContentKindToolUse marks a tool-use block.
	AgentContentKindToolUse AgentContentKind = "ToolUse"
)

// AgentContentPart is one decoded Zed assistant content block.
type AgentContentPart struct {
	Kind      AgentContentKind
	Text      string
	Signature string
	ToolUse   *ToolUse
}

// ToolUse is the persisted shape of one Zed tool-use block.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is the persisted shape of one Zed tool result.
type ToolResult struct {
	ToolUseID string            `json:"tool_use_id"`
	IsError   bool              `json:"is_error"`
	Content   []json.RawMessage `json:"content"`
	Output    json.RawMessage   `json:"output"`
}

// CompactionMessageKind identifies one Zed compaction variant.
type CompactionMessageKind string

const (
	// CompactionMessageKindSummary marks a summary compaction block.
	CompactionMessageKindSummary CompactionMessageKind = "Summary"
	// CompactionMessageKindProviderNative marks provider-native compaction items.
	CompactionMessageKindProviderNative CompactionMessageKind = "ProviderNative"
)

// CompactionMessage is the decoded Zed compaction union.
type CompactionMessage struct {
	Kind     CompactionMessageKind
	Summary  string
	Provider string
	Items    []json.RawMessage
}

// ParseCurrentThreadJSON decodes one current-version Zed thread payload.
func ParseCurrentThreadJSON(data []byte) (ThreadDocument, error) {
	var thread ThreadDocument
	if err := json.Unmarshal(data, &thread); err != nil {
		slog.Warn("providers.zed.store.current_thread_decode_failed", "concern", "providers.zed.store", "err", err)
		return ThreadDocument{}, fmt.Errorf("decode current zed thread json: %w", err)
	}
	if thread.Version != CurrentThreadVersion {
		return ThreadDocument{}, fmt.Errorf("unsupported current zed thread version %q", thread.Version)
	}
	return thread, nil
}

// UnmarshalJSON decodes the tagged Zed message union into one ThreadMessage.
func (message *ThreadMessage) UnmarshalJSON(data []byte) error {
	*message = ThreadMessage{Kind: "", User: nil, Agent: nil, Compaction: nil}
	var resume string
	if err := json.Unmarshal(data, &resume); err == nil && resume == "Resume" {
		message.Kind = ThreadMessageKindResume
		return nil
	}
	kind, payload, err := decodeVariant(data)
	if err != nil {
		return err
	}
	switch kind {
	case variantKindUser:
		var user UserMessage
		if err := json.Unmarshal(payload, &user); err != nil {
			return fmt.Errorf("decode zed user message: %w", err)
		}
		message.Kind, message.User = ThreadMessageKindUser, &user
	case variantKindAgent:
		var agent AgentMessage
		if err := json.Unmarshal(payload, &agent); err != nil {
			return fmt.Errorf("decode zed agent message: %w", err)
		}
		message.Kind, message.Agent = ThreadMessageKindAgent, &agent
	case variantKindCompaction:
		var compaction CompactionMessage
		if err := json.Unmarshal(payload, &compaction); err != nil {
			return fmt.Errorf("decode zed compaction message: %w", err)
		}
		message.Kind, message.Compaction = ThreadMessageKindCompaction, &compaction
	default:
		return fmt.Errorf("unsupported zed thread message variant %q", kind)
	}
	return nil
}

// UnmarshalJSON decodes one tagged Zed user content union block.
func (part *UserContentPart) UnmarshalJSON(data []byte) error {
	*part = UserContentPart{Kind: "", Text: "", Mention: nil}
	kind, payload, err := decodeVariant(data)
	if err != nil {
		return err
	}
	part.Kind = UserContentKind(kind)
	switch part.Kind {
	case UserContentKindText:
		if err := json.Unmarshal(payload, &part.Text); err != nil {
			return fmt.Errorf("decode zed user text part: %w", err)
		}
		return nil
	case UserContentKindMention:
		var mention MentionPart
		if err := json.Unmarshal(payload, &mention); err != nil {
			return fmt.Errorf("decode zed mention part: %w", err)
		}
		part.Mention = &mention
		return nil
	default:
		return fmt.Errorf("unsupported user content variant %q", kind)
	}
}

// UnmarshalJSON decodes one tagged Zed assistant content union block.
func (part *AgentContentPart) UnmarshalJSON(data []byte) error {
	*part = AgentContentPart{Kind: "", Text: "", Signature: "", ToolUse: nil}
	kind, payload, err := decodeVariant(data)
	if err != nil {
		return err
	}
	part.Kind = AgentContentKind(kind)
	switch part.Kind {
	case AgentContentKindText, AgentContentKindRedactedThinking:
		if err := json.Unmarshal(payload, &part.Text); err != nil {
			return fmt.Errorf("decode zed assistant text part: %w", err)
		}
		return nil
	case AgentContentKindThinking:
		var thinking struct {
			Text      string `json:"text"`
			Signature string `json:"signature"`
		}
		if err := json.Unmarshal(payload, &thinking); err != nil {
			return fmt.Errorf("decode zed thinking part: %w", err)
		}
		part.Text, part.Signature = thinking.Text, thinking.Signature
		return nil
	case AgentContentKindToolUse:
		var toolUse ToolUse
		if err := json.Unmarshal(payload, &toolUse); err != nil {
			return fmt.Errorf("decode zed tool use part: %w", err)
		}
		part.ToolUse = &toolUse
		return nil
	default:
		return fmt.Errorf("unsupported zed assistant content variant %q", kind)
	}
}

// UnmarshalJSON decodes one tagged Zed compaction union block.
func (message *CompactionMessage) UnmarshalJSON(data []byte) error {
	*message = CompactionMessage{Kind: "", Summary: "", Provider: "", Items: nil}
	kind, payload, err := decodeVariant(data)
	if err != nil {
		return err
	}
	message.Kind = CompactionMessageKind(kind)
	switch message.Kind {
	case CompactionMessageKindSummary:
		if err := json.Unmarshal(payload, &message.Summary); err != nil {
			return fmt.Errorf("decode zed compaction summary: %w", err)
		}
		return nil
	case CompactionMessageKindProviderNative:
		var providerNative struct {
			Provider string            `json:"provider"`
			Items    []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(payload, &providerNative); err != nil {
			return fmt.Errorf("decode zed provider-native compaction: %w", err)
		}
		message.Provider, message.Items = providerNative.Provider, providerNative.Items
		return nil
	default:
		return fmt.Errorf("unsupported zed compaction variant %q", kind)
	}
}

func decodeVariant(data []byte) (variantKind, json.RawMessage, error) {
	var variants map[string]json.RawMessage
	if err := json.Unmarshal(data, &variants); err != nil {
		slog.Warn("providers.zed.store.variant_decode_failed", "concern", "providers.zed.store", "err", err)
		return variantKind(""), nil, fmt.Errorf("decode zed enum variant: %w", err)
	}
	if len(variants) != 1 {
		return variantKind(""), nil, fmt.Errorf("expected single-key zed enum variant, got %d keys", len(variants))
	}
	for kind, payload := range variants {
		return variantKind(kind), payload, nil
	}
	return variantKind(""), nil, fmt.Errorf("expected single-key zed enum variant")
}
