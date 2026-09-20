// Package exportargs declares the conversation export arguments for every
// surface that accepts them: the terminal command, the MCP tool, and the
// compaction argument reader.
//
// This package imports internal/conversation alone. internal/clispec imports
// internal/daemon, and internal/daemon imports internal/reorientinject, so an
// import of internal/clispec from the compaction path creates an import cycle.
package exportargs

import (
	"goodkind.io/clyde/internal/conversation"
)

// Kind is the value shape of one argument.
type Kind uint8

const (
	// KindString is free text.
	KindString Kind = iota
	// KindInt is a whole number.
	KindInt
	// KindBool is an on or off switch.
	KindBool
	// KindEnum is a string constrained to Values.
	KindEnum
	// KindEnumList is a comma-separated list constrained to Values.
	KindEnumList
)

// Declaration is one export argument. It states no storage location, because
// each surface stores a parsed value in its own input struct.
type Declaration struct {
	// Canonical is the snake_case name. The terminal flag dash-spells it, and
	// the MCP property uses it verbatim.
	Canonical   string
	Kind        Kind
	Description string
	// Required rejects a command or a tool call that omits this argument.
	Required    bool
	Values      []string
	DefaultStr  string
	DefaultInt  int
	DefaultBool bool
	// CLIOnly keeps this argument off the MCP surface.
	CLIOnly bool
}

func stringDeclaration(canonical, description string) Declaration {
	return Declaration{
		Canonical: canonical, Kind: KindString, Description: description, Required: false,
		Values: nil, DefaultStr: "", DefaultInt: 0, DefaultBool: false, CLIOnly: false,
	}
}

func intDeclaration(canonical, description string) Declaration {
	return Declaration{
		Canonical: canonical, Kind: KindInt, Description: description, Required: false,
		Values: nil, DefaultStr: "", DefaultInt: 0, DefaultBool: false, CLIOnly: false,
	}
}

func boolDeclaration(canonical, description string, cliOnly bool) Declaration {
	return Declaration{
		Canonical: canonical, Kind: KindBool, Description: description, Required: false,
		Values: nil, DefaultStr: "", DefaultInt: 0, DefaultBool: false, CLIOnly: cliOnly,
	}
}

func enumDeclaration(canonical, description, defaultValue string, values []string) Declaration {
	return Declaration{
		Canonical: canonical, Kind: KindEnum, Description: description, Required: false,
		Values: values, DefaultStr: defaultValue, DefaultInt: 0, DefaultBool: false, CLIOnly: false,
	}
}

func enumListDeclaration(canonical, description string, values []string, required bool) Declaration {
	return Declaration{
		Canonical: canonical, Kind: KindEnumList, Description: description, Required: required,
		Values: values, DefaultStr: "", DefaultInt: 0, DefaultBool: false, CLIOnly: false,
	}
}

var whitespaceEnumValues = []string{
	string(conversation.WhitespacePreserve),
	string(conversation.WhitespaceTidy),
	string(conversation.WhitespaceCompact),
	string(conversation.WhitespaceDense),
}

// Compact has no shortcut. internal/clispec/export_flags_test.go asserts that
// the terminal declares no compact flag.
var whitespaceShortcutModes = []conversation.WhitespaceMode{
	conversation.WhitespacePreserve,
	conversation.WhitespaceTidy,
	conversation.WhitespaceDense,
}

var exportFormatValues = []string{
	string(conversation.ExportFormatMarkdown),
	string(conversation.ExportFormatHTML),
	string(conversation.ExportFormatJSON),
	string(conversation.ExportFormatPlainText),
}

var contentShortcutDescriptions = map[string]string{
	"chat":              "Include conversation chat text.",
	"thinking":          "Include assistant thinking blocks.",
	"tool_calls":        "Include tool calls.",
	"tool_outputs":      "Include tool result bodies.",
	"system_prompts":    "Include system-injected prompts.",
	"system_messages":   "Include provider system transcript records.",
	"injected":          "Include hook-pushed context inside user messages.",
	"raw_json_metadata": "Include JSON metadata fields.",
	"tools":             "Include summary-only tool lines.",
	"all":               "Include every non-tool kind plus tool outputs.",
}

// Declarations returns the export arguments in a stable order.
func Declarations() []Declaration {
	declarations := []Declaration{
		enumDeclaration("format", "markdown, html, json, or plain_text.",
			string(conversation.ExportFormatMarkdown), exportFormatValues),
		enumDeclaration("whitespace", "preserve, tidy, compact, or dense.", "", whitespaceEnumValues),
		intDeclaration("history_start", "First message index to include."),
		stringDeclaration("include_compactions",
			"Compaction segments to export: 0, 0,1, 0..2, or all. Defaults to 0."),
		boolDeclaration("full_history",
			"Export all compaction segments. Equivalent to --include-compactions all.", false),
		intDeclaration("last_n",
			"Keep only the last N visible messages after compaction segment selection."),
		intDeclaration("max_lines",
			"Keep only the last N rendered lines after whitespace compression. Zero leaves the output uncapped."),
		stringDeclaration("max_tokens",
			"Cap the rendered body to a token budget, keeping the tail. Accepts human sizes like 200000, 200,000, 200k, or 1m. Empty leaves the output uncapped."),
		stringDeclaration("token_model",
			"Override the model whose tokenizer counts --max-tokens (for example gpt-4o). Empty derives it from the conversation's provider and model."),
		enumListDeclaration("only",
			"Content kinds to export, comma-separated: chat, thinking, tools, tool_calls, tool_outputs, system_prompts, system_messages, injected, raw_json_metadata, plus all.",
			conversation.ContentKindSelectorValues(), true),
	}
	for _, mode := range whitespaceShortcutModes {
		declarations = append(declarations,
			boolDeclaration(string(mode), "Use "+string(mode)+" whitespace.", true))
	}
	for _, selector := range conversation.ContentKindSelectorValues() {
		declarations = append(declarations,
			boolDeclaration(selector, contentShortcutDescription(selector), true))
	}
	return declarations
}

func contentShortcutDescription(selector string) string {
	if described, ok := contentShortcutDescriptions[selector]; ok {
		return described
	}
	return "Include " + selector + " content."
}
