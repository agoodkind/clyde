package cursorstore

import (
	"encoding/json"
	"strings"
	"time"
)

const bubbleKeyPrefix = "bubbleId:"

// OrderableTimestamp returns a bubble's write time when it is one this build can
// read, and the empty string when it is not.
//
// Ordering compares these as strings, which is sound only for the fixed-width
// RFC3339 form Cursor writes. A value in any other shape is not a later time, it
// is an unknown one, and the renderer already treats it that way. Passing it
// through would give the bubble a definite interior position while the message it
// becomes carries no time at all, so the order and the transcript would disagree
// about the same field. Measured on a real store, all 536,860 non-empty values
// are in the expected shape, so this rejects nothing there.
func OrderableTimestamp(createdAt string) string {
	if createdAt == "" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		return ""
	}
	return createdAt
}

const (
	// BubbleTypeUser identifies a user-authored Cursor composer bubble.
	BubbleTypeUser int = 1
	// BubbleTypeAssistant identifies an assistant-authored Cursor composer bubble.
	BubbleTypeAssistant int = 2
)

// Bubble models the consumed subset of Cursor's undocumented, version-pinned
// on-disk bubble payload. Unconsumed fields are intentionally ignored.
type Bubble struct {
	BubbleID      string
	Type          int
	SchemaVersion int
	Text          string
	Thinking      BubbleThinking
	ToolCall      *BubbleToolCall
	// CreatedAt is Cursor's RFC3339 write time for the bubble. It is the only
	// ordering signal available for a bubble the composer header does not
	// reference, and it is compared as a string because the format is fixed-width
	// and sorts lexicographically.
	CreatedAt string
	// ServerBubbleID is Cursor's server-side identity for the message. Cursor
	// rewrites a turn under a new local bubble id and leaves the old row behind,
	// but both rows keep the same server id, so it is what tells a superseded copy
	// apart from a genuinely repeated turn. Not every row carries one.
	ServerBubbleID string
}

// HasContent reports whether a bubble carries anything a reader would see. A
// composer stores many bubbles with no text, thinking, or tool call at all.
func (bubble Bubble) HasContent() bool {
	return strings.TrimSpace(bubble.Text) != "" ||
		strings.TrimSpace(bubble.Thinking.Text) != "" ||
		bubble.ToolCall != nil
}

// BubbleThinking models the consumed assistant reasoning fields in a Cursor
// bubble payload.
type BubbleThinking struct {
	Text string `json:"text"`
}

// BubbleToolCall models the consumed tool call fields in a Cursor bubble
// payload.
type BubbleToolCall struct {
	Name    string           `json:"name"`
	RawArgs string           `json:"rawArgs"`
	Result  BubbleToolResult `json:"result"`
	Status  string           `json:"status"`
	// ToolCallID is the model's own identifier for the call, such as
	// `call_eZCEfOnkc` or a `toolu_` id. Cursor writes it on 98.4% of the tool
	// rows in a real store, and it is what separates one call from another when
	// two rows would otherwise read identically.
	ToolCallID string `json:"toolCallId"`
}

// BubbleToolResult is a tool's result as Cursor stores it. The on-disk format is
// a union: most tools write the result as a JSON string, and some write a JSON
// object instead. Measured on a real store, 1.03% of bubbles carry the object
// form, and a decoder that accepts only the string form fails the whole bubble
// and, before this type existed, the whole conversation read with it.
//
// This is the one place that opacity is allowed to live. The object form is kept
// as its raw JSON text so no tool output is lost, and every reader downstream
// sees a plain string.
type BubbleToolResult string

// UnmarshalJSON accepts either spelling of Cursor's tool result.
func (result *BubbleToolResult) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*result = BubbleToolResult(text)
		return nil
	}
	*result = BubbleToolResult(data)
	return nil
}

type bubbleWire struct {
	SchemaVersion  int             `json:"_v"`
	Type           int             `json:"type"`
	BubbleID       string          `json:"bubbleId"`
	Text           string          `json:"text"`
	Thinking       BubbleThinking  `json:"thinking"`
	ToolCall       *BubbleToolCall `json:"toolFormerData"`
	CreatedAt      string          `json:"createdAt"`
	ServerBubbleID string          `json:"serverBubbleId"`
}

// DecodeBubbleJSON decodes one Cursor bubble value. A bubble whose `_v` schema
// version differs from the expected version is still decoded best-effort
// because the consumed fields are stable across versions; callers can detect a
// mismatch via the returned SchemaVersion.
func DecodeBubbleJSON(data []byte) (Bubble, error) {
	var wire bubbleWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return Bubble{}, CursorJSONDecodeError{Description: "bubble", Err: err}
	}
	return Bubble{
		BubbleID:       wire.BubbleID,
		Type:           wire.Type,
		SchemaVersion:  wire.SchemaVersion,
		Text:           wire.Text,
		Thinking:       wire.Thinking,
		ToolCall:       wire.ToolCall,
		CreatedAt:      wire.CreatedAt,
		ServerBubbleID: wire.ServerBubbleID,
	}, nil
}

// bubbleKey builds the `bubbleId:<composerId>:<bubbleId>` key one bubble row is
// stored under, so a reader that already knows which bubble it wants seeks the
// primary key instead of scanning the chat's range again.
func bubbleKey(composerID string, bubbleID string) string {
	return bubbleKeyPrefix + composerID + ":" + bubbleID
}

// parseBubbleKey splits a `bubbleId:<composerId>:<bubbleId>` key into the two
// identifiers it carries, and reports false for a key that is not a bubble key.
// It lives beside the key the format is built from, so the format is written in
// one place and read in one place.
func parseBubbleKey(key string) (string, string, bool) {
	rest, ok := strings.CutPrefix(key, bubbleKeyPrefix)
	if !ok {
		return "", "", false
	}
	composerID, bubbleID, ok := strings.Cut(rest, ":")
	if !ok || composerID == "" || bubbleID == "" {
		return "", "", false
	}
	return composerID, bubbleID, true
}

// bubbleIDFromKey narrows [parseBubbleKey] to one known chat, for a reader that
// walks that chat's key range and has only the key to tell it which bubble a row
// carries.
func bubbleIDFromKey(key string, composerID string) (string, bool) {
	keyComposerID, bubbleID, ok := parseBubbleKey(key)
	if !ok || keyComposerID != composerID {
		return "", false
	}
	return bubbleID, true
}
