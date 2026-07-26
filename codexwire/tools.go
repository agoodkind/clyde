package codexwire

import "encoding/json"

// ToolSpec is the typed wire shape for one entry in the Codex
// Responses `tools` array. Two variants are in scope: the function
// tool, and the custom (freeform) tool whose payload is raw text
// constrained by a grammar. The upstream's built-in tools (web_search,
// file_search, etc.) are out of scope for this package.
//
// Only the fields valid for the active Type are populated. Parameters
// and Strict belong to the function variant, Format belongs to the
// custom variant, and omitempty trims the rest at serialization time.
type ToolSpec struct {
	Type        ToolSpecType    `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
	Format      *ToolFormat     `json:"format,omitempty"`
}

// ToolSpecType is the closed enum of tool-spec types this package
// emits.
type ToolSpecType string

const (
	// ToolSpecTypeFunction is a JSON-arguments tool.
	ToolSpecTypeFunction ToolSpecType = "function"
	// ToolSpecTypeCustom is a freeform tool whose payload is raw text.
	// The upstream answers one with a custom_tool_call item carrying
	// `input` rather than a function_call carrying `arguments`.
	ToolSpecTypeCustom ToolSpecType = "custom"
)

// ToolFormat constrains the freeform payload of a custom tool. A
// grammar format carries the grammar syntax and definition; a text
// format carries neither.
type ToolFormat struct {
	Type       ToolFormatType `json:"type"`
	Syntax     string         `json:"syntax,omitempty"`
	Definition string         `json:"definition,omitempty"`
}

// ToolFormatType is the closed enum of custom-tool format types.
type ToolFormatType string

const (
	// ToolFormatTypeText is an unconstrained freeform payload.
	ToolFormatTypeText ToolFormatType = "text"
	// ToolFormatTypeGrammar constrains the payload to a grammar.
	ToolFormatTypeGrammar ToolFormatType = "grammar"
)
