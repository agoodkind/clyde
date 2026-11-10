package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// cursorApplyPatchGrammar is the lark grammar Cursor declares on its
// ApplyPatch custom tool, trimmed to its start rules.
const cursorApplyPatchGrammar = "start: begin_patch hunk end_patch\n" +
	"begin_patch: \"*** Begin Patch\" LF\n" +
	"end_patch: \"*** End Patch\" LF?\n"

// cursorApplyPatchToolJSON is the tools-array entry Cursor sends for its
// ApplyPatch tool, matching a captured Cursor BYOK request.
const cursorApplyPatchToolJSON = `{"type":"custom","name":"ApplyPatch",` +
	`"description":"Use this tool to edit files.",` +
	`"format":{"type":"grammar","syntax":"lark",` +
	`"definition":"start: begin_patch hunk end_patch\nbegin_patch: \"*** Begin Patch\" LF\nend_patch: \"*** End Patch\" LF?\n"}}`

func TestToolUnmarshalOpenAIFreeformCustomKeepsGrammar(t *testing.T) {
	t.Parallel()

	var tool Tool
	if err := json.Unmarshal([]byte(cursorApplyPatchToolJSON), &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !ToolIsCustom(tool) {
		t.Fatalf("tool type = %q, want custom", tool.Type)
	}
	if tool.Function.Name != "ApplyPatch" {
		t.Fatalf("name = %q, want ApplyPatch", tool.Function.Name)
	}
	if tool.Function.Description != "Use this tool to edit files." {
		t.Fatalf("description = %q", tool.Function.Description)
	}
	if tool.Format == nil {
		t.Fatalf("format dropped, want the declared grammar")
	}
	if tool.Format.Type != ToolGrammarFormatGrammar {
		t.Fatalf("format type = %q, want grammar", tool.Format.Type)
	}
	if tool.Format.Syntax != "lark" {
		t.Fatalf("format syntax = %q, want lark", tool.Format.Syntax)
	}
	if tool.Format.Definition != cursorApplyPatchGrammar {
		t.Fatalf("format definition = %q", tool.Format.Definition)
	}
	if len(tool.Function.Parameters) != 0 {
		t.Fatalf("parameters = %q, want none on a freeform tool", tool.Function.Parameters)
	}
}

// A custom tool that declares a JSON schema is Anthropic's meaning of
// "custom" (an ordinary schema tool), not OpenAI's freeform tool, so it
// must keep projecting to a function tool.
func TestToolUnmarshalCustomWithSchemaStaysFunction(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"custom","name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"zip":{"type":"string"}}}}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ToolIsCustom(tool) {
		t.Fatalf("tool type = %q, want function for a schema-carrying custom tool", tool.Type)
	}
	if !strings.Contains(string(tool.Function.Parameters), `"zip"`) {
		t.Fatalf("parameters lost the declared schema: %s", tool.Function.Parameters)
	}
	if tool.Format != nil {
		t.Fatalf("format = %#v, want nil on a function tool", tool.Format)
	}
}

// A contradictory entry declaring both a grammar format and a JSON
// schema stays a function tool. The custom wire shape has no parameters
// field, so classifying it as custom would silently drop the schema.
func TestToolUnmarshalCustomWithBothFormatAndSchemaKeepsSchema(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"custom","name":"Ambiguous","description":"both",` +
		`"format":{"type":"grammar","syntax":"lark","definition":"start: /(.|\n)+/\n"},` +
		`"input_schema":{"type":"object","properties":{"zip":{"type":"string"}}},"strict":true}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ToolIsCustom(tool) {
		t.Fatalf("tool type = %q, want function when a schema is present", tool.Type)
	}
	if !strings.Contains(string(tool.Function.Parameters), `"zip"`) {
		t.Fatalf("schema dropped: %s", tool.Function.Parameters)
	}
	if tool.Function.Strict == nil || !*tool.Function.Strict {
		t.Fatalf("strict dropped: %v", tool.Function.Strict)
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"zip"`) {
		t.Fatalf("schema lost on re-serialization: %s", encoded)
	}
}

func TestToolMarshalFreeformCustomRoundTrips(t *testing.T) {
	t.Parallel()

	var tool Tool
	if err := json.Unmarshal([]byte(cursorApplyPatchToolJSON), &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Tool
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal round trip: %v", err)
	}
	if !ToolIsCustom(decoded) {
		t.Fatalf("round-tripped type = %q, want custom", decoded.Type)
	}
	if decoded.Format == nil || decoded.Format.Definition != cursorApplyPatchGrammar {
		t.Fatalf("round trip lost the grammar: %#v", decoded.Format)
	}
	if strings.Contains(string(encoded), `"function"`) {
		t.Fatalf("custom tool serialized under a function envelope: %s", encoded)
	}
}

func TestToolMarshalFunctionKeepsNestedShape(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object"}}}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	encoded, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"function":{`) {
		t.Fatalf("function tool lost its nested envelope: %s", encoded)
	}
	if strings.Contains(string(encoded), `"format"`) {
		t.Fatalf("function tool gained a format field: %s", encoded)
	}
}
