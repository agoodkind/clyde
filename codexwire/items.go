package codexwire

import (
	"encoding/json"
	"strings"
)

// InputItem is the typed wire envelope for one entry in a Codex
// Responses `input` array (request side) or in a captured
// output-item list (response side). Variants map 1:1 to the Rust
// ResponseItem enum at research/codex/codex-rs/protocol/src/models.rs.
//
// The struct carries every field any variant uses. Only the fields
// valid for the active Type are populated; omitempty trims the rest
// at serialization time so the wire payload matches the codex-rs
// shape. Output and Action are kept as raw JSON so the union shape
// the upstream uses for them (string or array for output; struct for
// action) round-trips without per-variant boxing.
type InputItem struct {
	Type             ItemType           `json:"type"`
	ID               string             `json:"id,omitempty"`
	Role             string             `json:"role,omitempty"`
	Content          ContentItems       `json:"content,omitempty"`
	CallID           string             `json:"call_id,omitempty"`
	Name             string             `json:"name,omitempty"`
	Arguments        string             `json:"arguments,omitempty"`
	Input            string             `json:"input,omitempty"`
	Output           json.RawMessage    `json:"output,omitempty"`
	Action           json.RawMessage    `json:"action,omitempty"`
	Status           string             `json:"status,omitempty"`
	Summary          []ReasoningSummary `json:"summary,omitempty"`
	EncryptedContent string             `json:"encrypted_content,omitempty"`
}

// ItemType is the closed enum of Codex history item type strings
// used across canonicalization, continuation parity, and SSE
// handlers.
type ItemType string

// Item type discriminator constants. The set matches the Rust
// ResponseItem variants we round-trip.
const (
	ItemTypeMessage              ItemType = "message"
	ItemTypeFunctionCall         ItemType = "function_call"
	ItemTypeLocalShellCall       ItemType = "local_shell_call"
	ItemTypeCustomToolCall       ItemType = "custom_tool_call"
	ItemTypeFunctionCallOutput   ItemType = "function_call_output"
	ItemTypeCustomToolCallOutput ItemType = "custom_tool_call_output"
	ItemTypeReasoning            ItemType = "reasoning"
)

// ContentItems is a typed slice of [ContentItem]. The named type
// lets callers spell the slice without leaking `any` in signatures.
type ContentItems []ContentItem

// ContentItem mirrors the codex-rs ContentItem enum. Only one of
// Text or ImageURL is populated per item; Detail is optional and
// only applies to input_image.
type ContentItem struct {
	Type     ContentItemType `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL string          `json:"image_url,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

// ContentItemType is the closed enum of content-part types inside a
// Codex Message item.
type ContentItemType string

// Content-item type discriminator constants.
const (
	ContentItemInputText  ContentItemType = "input_text"
	ContentItemInputImage ContentItemType = "input_image"
	ContentItemOutputText ContentItemType = "output_text"
	ContentItemPlainText  ContentItemType = "text"
)

// ReasoningSummary mirrors a Rust ReasoningItemReasoningSummary
// entry. The upstream type discriminator is fixed to "summary_text".
type ReasoningSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// LocalShellAction mirrors codex-rs LocalShellAction. The shape is
// decoded from [InputItem.Action] on inbound parsing of
// local_shell_call items.
type LocalShellAction struct {
	Type             string            `json:"type,omitempty"`
	Command          CommandValue      `json:"command,omitempty"`
	WorkingDirectory string            `json:"working_directory,omitempty"`
	Workdir          string            `json:"workdir,omitempty"`
	Cwd              string            `json:"cwd,omitempty"`
	TimeoutMs        *int64            `json:"timeout_ms,omitempty"`
	BlockUntilMs     *int64            `json:"block_until_ms,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	User             string            `json:"user,omitempty"`
}

// WorkingDir picks the first populated working-directory alias on
// the local-shell action (working_directory, workdir, cwd).
func (a LocalShellAction) WorkingDir() string {
	if v := strings.TrimSpace(a.WorkingDirectory); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.Workdir); v != "" {
		return v
	}
	return strings.TrimSpace(a.Cwd)
}

// CommandValue accepts either a string (e.g. "/bin/sh -lc foo") or
// a Vec<String> (argv array) on the wire. The decoder normalizes
// to argv-style []string; the encoder emits as a JSON array when
// multiple entries exist and as a single string otherwise.
type CommandValue []string

// UnmarshalJSON accepts either a JSON string ("foo bar") or a JSON
// array of strings (["foo","bar"]).
func (c *CommandValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*c = nil
		return nil
	}
	if trimmed[0] == '[' {
		var argv []string
		if err := json.Unmarshal(data, &argv); err != nil {
			return wrapJSONErr("codexwire local_shell command argv", err)
		}
		*c = argv
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return wrapJSONErr("codexwire local_shell command string", err)
	}
	if single == "" {
		*c = nil
	} else {
		*c = []string{single}
	}
	return nil
}

// MarshalJSON emits a single-string scalar when the argv has one
// entry and a JSON array when it has more. Empty values are skipped
// by the parent field's omitempty.
func (c CommandValue) MarshalJSON() ([]byte, error) {
	if len(c) == 0 {
		return []byte("null"), nil
	}
	if len(c) == 1 {
		out, err := json.Marshal(c[0])
		if err != nil {
			return nil, wrapJSONErr("codexwire local_shell command marshal", err)
		}
		return out, nil
	}
	out, err := json.Marshal([]string(c))
	if err != nil {
		return nil, wrapJSONErr("codexwire local_shell command argv marshal", err)
	}
	return out, nil
}

// Text joins the text fields of every content item. Image and
// unknown items contribute nothing.
func (c ContentItems) Text() string {
	if len(c) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c))
	for _, item := range c {
		switch item.Type {
		case ContentItemPlainText, ContentItemInputText, ContentItemOutputText:
			if item.Text != "" {
				parts = append(parts, item.Text)
			}
		case ContentItemInputImage:
			continue
		}
	}
	return strings.Join(parts, "\n")
}

// OutputText flattens an [InputItem.Output] value to plain text.
// Codex's function_call_output.output is a union over a JSON string
// and an array of content items; this helper picks the string or
// joins the text fields of the array.
func OutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	if trimmed[0] == '[' {
		var items ContentItems
		if err := json.Unmarshal(raw, &items); err != nil {
			return ""
		}
		return items.Text()
	}
	return ""
}

// RawOutput encodes a plain string body into the
// function_call_output `output` field shape (a JSON string). Use
// this when building outbound items from plain text.
func RawOutput(text string) json.RawMessage {
	body, err := json.Marshal(text)
	if err != nil {
		return nil
	}
	return body
}

// wrapJSONErr attaches a short label to a JSON encode/decode error.
func wrapJSONErr(label string, err error) error {
	if err == nil {
		return nil
	}
	return &jsonOpError{Label: label, Err: err}
}

type jsonOpError struct {
	Label string
	Err   error
}

func (e *jsonOpError) Error() string {
	return e.Label + ": " + e.Err.Error()
}

func (e *jsonOpError) Unwrap() error { return e.Err }
