package transcript

import (
	"encoding/json"
	"time"
)

// Message represents a single parsed conversation turn. It is the generic model
// the provider parsers produce and the renderers consume; the transcript package
// owns the model and the rendering, not any provider's parsing.
type Message struct {
	UUID              string              `json:"uuid"`                 // entry UUID (for linking to tool results)
	ParentUUID        string              `json:"parent_uuid"`          // direct provider parent entry UUID when present
	LogicalParentUUID string              `json:"logical_parent_uuid"`  // provider logical parent UUID when present
	Role              string              `json:"role"`                 // "user", "assistant", or provider-internal "system"
	Visibility        MessageVisibility   `json:"visibility"`           // provider visibility hint for this entry
	Compaction        *CompactionMetadata `json:"compaction,omitempty"` // typed compaction-boundary metadata when present
	Timestamp         time.Time           `json:"timestamp"`            // when this entry was created
	Text              string              `json:"text"`                 // concatenated text blocks (no tool calls, no thinking)
	Thinking          string              `json:"thinking"`             // thinking block text (for HTML export)
	HasTools          bool                `json:"has_tools"`            // true if assistant message contained tool_use blocks
	Tools             []ToolCall          `json:"tools,omitempty"`      // parsed tool calls with inputs
	// HarnessStrips counts what the provider parser removed from Text under the
	// load options, so a consumer can report stripping without knowing any
	// provider's markers. It never serializes: it describes this parse, not the
	// conversation.
	HarnessStrips HarnessStrips `json:"-"`
}

// HarnessStrips counts harness-written content a parser removed from one
// message's text: injected spans (hook-pushed context) and system spans
// (harness-native tags). Zero values mean nothing was removed.
type HarnessStrips struct {
	Injected int
	System   int
}

// MessageVisibility is part of Clyde's typed adapter surface.
type MessageVisibility string

const (
	// MessageVisibilityVisible marks entries that are part of the visible chat.
	MessageVisibilityVisible MessageVisibility = "visible"
	// MessageVisibilityTranscriptOnly marks entries that appear only in transcript views.
	MessageVisibilityTranscriptOnly MessageVisibility = "transcript_only"
	// MessageVisibilityMetaOnly marks entries that are metadata-only control records.
	MessageVisibilityMetaOnly MessageVisibility = "meta_only"
)

// CompactionKind is part of Clyde's typed adapter surface.
type CompactionKind string

const (
	// CompactionKindSummary marks a user-visible compact summary.
	CompactionKindSummary CompactionKind = "summary"
	// CompactionKindBoundary marks a provider compaction boundary.
	CompactionKindBoundary CompactionKind = "boundary"
	// CompactionKindMicroboundary marks a provider micro-compaction boundary.
	CompactionKindMicroboundary CompactionKind = "microboundary"
)

// CompactionTrigger is part of Clyde's typed adapter surface.
type CompactionTrigger string

const (
	// CompactionTriggerManual marks a manually-triggered compaction.
	CompactionTriggerManual CompactionTrigger = "manual"
	// CompactionTriggerAuto marks an automatically-triggered compaction.
	CompactionTriggerAuto CompactionTrigger = "auto"
	// CompactionTriggerUnknown marks a compaction whose trigger is not known.
	CompactionTriggerUnknown CompactionTrigger = "unknown"
)

// CompactionMetadata is part of Clyde's typed adapter surface.
type CompactionMetadata struct {
	Kind                      CompactionKind         `json:"kind"`
	Trigger                   CompactionTrigger      `json:"trigger"`
	PreTokens                 int                    `json:"pre_tokens"`
	PostTokens                int                    `json:"post_tokens"`
	TokensSaved               int                    `json:"tokens_saved"`
	MessagesSummarized        int                    `json:"messages_summarized"`
	ReplacementHistoryCount   int                    `json:"replacement_history_count"`
	HeadUUID                  string                 `json:"head_uuid"`
	AnchorUUID                string                 `json:"anchor_uuid"`
	TailUUID                  string                 `json:"tail_uuid"`
	ContextItems              []CompactedContextItem `json:"context_items,omitempty"`
	UserContext               string                 `json:"user_context"`
	Direction                 string                 `json:"direction"`
	PreCompactDiscoveredTools []string               `json:"pre_compact_discovered_tools,omitempty"`
	CompactedToolIDs          []string               `json:"compacted_tool_ids,omitempty"`
	ClearedAttachmentUUIDs    []string               `json:"cleared_attachment_uuids,omitempty"`
	RawCompactMetadata        json.RawMessage        `json:"raw_compact_metadata,omitempty"`
	RawMicrocompactMetadata   json.RawMessage        `json:"raw_microcompact_metadata,omitempty"`
	RawSummarizeMetadata      json.RawMessage        `json:"raw_summarize_metadata,omitempty"`
}

// ToolInputJSON keeps a tool_use.input payload opaque at the model boundary.
// Tool inputs vary by tool and mirror the upstream transcript content-block
// shape; the transcript package only stores and re-renders this JSON, so any
// business logic should decode a concrete schema at a narrower call site.
type ToolInputJSON struct {
	Raw json.RawMessage `json:"raw"`
}

// UnmarshalJSON is part of Clyde's typed adapter surface.
func (j *ToolInputJSON) UnmarshalJSON(data []byte) error {
	if j == nil {
		return nil
	}
	j.Raw = append(j.Raw[:0], data...)
	return nil
}

// MarshalJSON is part of Clyde's typed adapter surface.
func (j *ToolInputJSON) MarshalJSON() ([]byte, error) {
	if j == nil {
		return []byte("null"), nil
	}
	if len(j.Raw) == 0 {
		return []byte("null"), nil
	}
	return j.Raw, nil
}

// Len is part of Clyde's typed adapter surface.
func (j *ToolInputJSON) Len() int {
	if j == nil {
		return 0
	}
	return len(j.Raw)
}

// ToolCall represents a single tool invocation within an assistant message.
type ToolCall struct {
	ID    string        `json:"id"`    // tool_use_id (links to tool_result in next user message)
	Name  string        `json:"name"`  // e.g. "Bash", "Edit", "Read"
	Input ToolInputJSON `json:"input"` // opaque tool input payload, preserved verbatim
	// Display is what the user saw for this call: a shell command, a file path,
	// a search pattern. Each provider's parser fills it from its own harness's
	// tool shapes. Anything that shows or stores a tool call reads this rather than
	// re-deriving it from Input, which keeps the harness's serialization inside the
	// provider package.
	Display string `json:"display,omitempty"`
	// DisplayLang names the language Display is written in, "bash" when the call
	// ran a shell and empty otherwise. The same parser names the language from
	// its own harness's tool shapes. Anything that shows or stores a tool call
	// reads this rather than re-deriving it from Input, which keeps the harness's
	// serialization inside the provider package.
	DisplayLang string `json:"display_lang,omitempty"`
	Output      string `json:"output"`   // tool result text (loaded on demand, empty by default)
	IsError     bool   `json:"is_error"` // true if tool result was an error
}

// ToolNames returns the names of all tools used in this message.
func (m *Message) ToolNames() []string {
	names := make([]string, len(m.Tools))
	for i, t := range m.Tools {
		names[i] = t.Name
	}
	return names
}

// CompactionMessages returns only messages that carry typed compaction metadata.
func CompactionMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, message := range messages {
		if message.Compaction == nil {
			continue
		}
		out = append(out, message)
	}
	return out
}
