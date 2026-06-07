package transcript

import (
	"encoding/json"
	"time"
)

// Message represents a single parsed conversation turn. It is the generic model
// the provider parsers produce and the renderers consume; the transcript package
// owns the model and the rendering, not any provider's parsing.
type Message struct {
	UUID      string     // entry UUID (for linking to tool results)
	Role      string     // "user" or "assistant"
	Timestamp time.Time  // when this entry was created
	Text      string     // concatenated text blocks (no tool calls, no thinking)
	Thinking  string     // thinking block text (for HTML export)
	HasTools  bool       // true if assistant message contained tool_use blocks
	Tools     []ToolCall // parsed tool calls with inputs
}

// ToolInputJSON keeps a tool_use.input payload opaque at the model boundary.
// Tool inputs vary by tool and mirror the upstream transcript content-block
// shape; the transcript package only stores and re-renders this JSON, so any
// business logic should decode a concrete schema at a narrower call site.
type ToolInputJSON struct {
	Raw json.RawMessage
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
	ID      string        // tool_use_id (links to tool_result in next user message)
	Name    string        // e.g. "Bash", "Edit", "Read"
	Input   ToolInputJSON // opaque tool input payload, preserved verbatim
	Output  string        // tool result text (loaded on demand, empty by default)
	IsError bool          // true if tool result was an error
}

// ToolNames returns the names of all tools used in this message.
func (m *Message) ToolNames() []string {
	names := make([]string, len(m.Tools))
	for i, t := range m.Tools {
		names[i] = t.Name
	}
	return names
}
