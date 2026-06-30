package codexwire

import "encoding/json"

// ToolSpec is the typed wire shape for one entry in the Codex
// Responses `tools` array. Only the function-tool variant is in
// scope here; the upstream's built-in tools (web_search,
// file_search, etc.) are out of scope for this package.
type ToolSpec struct {
	Type        ToolSpecType    `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolSpecType is the closed enum of tool-spec types this package
// emits.
type ToolSpecType string

// ToolSpecTypeFunction is the only currently-emitted tool spec
// type.
const ToolSpecTypeFunction ToolSpecType = "function"
