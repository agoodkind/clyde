package contextcount

import "encoding/json"

// Transcript is the provider-neutral input shape passed to a Counter.
// It preserves the model, system text blocks, tool definitions, and
// ordered chat messages that a provider would see for a session.
type Transcript struct {
	Model    string
	System   []TextBlock
	Tools    []ToolDef
	Messages []Message
}

// TextBlock is a textual system block.
type TextBlock struct {
	Text string
}

// ToolDef describes a tool made available to the transcript.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Message is a single user or assistant message.
type Message struct {
	Role    string
	Content []ContentBlock
}

// ContentBlock preserves one provider content block without collapsing
// block-specific fields into a lossy text-only representation.
type ContentBlock struct {
	Type           string
	Text           string
	Thinking       string
	Signature      string
	RedactedData   string
	ToolUseID      string
	ToolName       string
	ToolInput      json.RawMessage
	ToolResult     []ContentBlock
	ToolResultText string
	IsError        bool
	ImageSource    ImageSource
}

// ImageSource describes either a base64-backed or URL-backed image block.
type ImageSource struct {
	Type      string
	MediaType string
	Data      string
	URL       string
}
