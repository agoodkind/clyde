// Package searchbackend projects Clyde conversation messages into the immutable
// occurrence fields that embedded conversation search stores, and compares them
// with the fields a previous ingestion committed.
package searchbackend

import "time"

// FieldKind is the selected part of a message that one field stores. Every row
// key includes the kind. Never change the meaning of an existing value.
type FieldKind string

const (
	// FieldKindChat is the message text.
	FieldKindChat FieldKind = "chat"
	// FieldKindThinking is the message reasoning text.
	FieldKindThinking FieldKind = "thinking"
	// FieldKindToolName is one tool call reduced to its tool name, which the
	// tool summaries content kind selects.
	FieldKindToolName FieldKind = "tool_name"
	// FieldKindToolCall is one tool call with the text the user saw and the
	// shell programs and file targets parsed from a shell display.
	FieldKindToolCall FieldKind = "tool_call"
	// FieldKindToolOutput is the text one tool call returned.
	FieldKindToolOutput FieldKind = "tool_output"
)

// ToolDetail is the tool content level the configured content kinds select.
type ToolDetail uint8

const (
	// ToolDetailNone selects no tool field.
	ToolDetailNone ToolDetail = iota
	// ToolDetailName selects the tool name alone.
	ToolDetailName
	// ToolDetailCall selects the tool call with its displayed text.
	ToolDetailCall
	// ToolDetailOutput selects the tool call and the text the tool returned.
	ToolDetailOutput
)

// projectionProfileVersion identifies the field construction rules in this
// package. A rule change ships as a new version. The new version gives every
// field a new row key, and rows committed under the earlier version stay in
// the catalog.
const projectionProfileVersion = "p2"

// Conversation is one loaded conversation and the projection settings for it.
type Conversation struct {
	// ID is the stable Clyde conversation ID, which is the occurrence owner.
	ID string
	// LoadRules is the loading-rules tag that produced the message positions.
	LoadRules string
	// MessageCount is the length of the complete loaded message sequence,
	// including messages that select no content.
	MessageCount int
	// TrailingMessageMayGrow reports that the provider can extend the last
	// loaded message in place when the artifact grows.
	TrailingMessageMayGrow bool
	// ArtifactSettled reports that the artifact stayed unchanged long enough
	// for the caller to commit a trailing message that may still grow.
	ArtifactSettled bool
	// ToolDetail is the selected tool content level.
	ToolDetail ToolDetail
}

// Tool is one tool call as the provider parser rendered it.
type Tool struct {
	Name        string
	Display     string
	DisplayLang string
	Output      string
}

// Message is one message with selected content, at its original position in
// the complete loaded sequence.
type Message struct {
	// Index is the message position in the complete loaded sequence, counted
	// before content selection.
	Index int
	// ProviderMessageID is the provider message identifier, or empty when the
	// provider records none. Projection state stores it for context checks.
	ProviderMessageID string
	Role              string
	Timestamp         time.Time
	Text              string
	Thinking          string
	Tools             []Tool
}

// Field is one selected message field before model-sized splitting.
type Field struct {
	// Key identifies the field within its conversation: projection profile,
	// message index, kind, and tool index for a tool field. Row keys append the
	// prepared part suffix to it.
	Key               string
	MessageIndex      int
	ProviderMessageID string
	Role              string
	Timestamp         time.Time
	Kind              FieldKind
	// ToolIndex is the tool call position within the message, or -1 for a
	// message field.
	ToolIndex int
	// DocumentPrefix starts every embedding input of the field. Tool fields
	// use tool attribution and derived search tokens.
	DocumentPrefix string
	// Text is the selected source text that splitting divides into parts.
	Text string
	// Digest is the SHA-256 of the length-prefixed DocumentPrefix and Text.
	Digest string
}
