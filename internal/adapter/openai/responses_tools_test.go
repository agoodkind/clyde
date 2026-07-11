package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSplitResponsesToolsKeepsFunctionDropsBuiltins(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`[` +
		`{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},` +
		`{"type":"web_search"},` +
		`{"type":"custom","name":"weather","input_schema":{"type":"object"}}` +
		`]`)
	functionTools, droppedTypes := SplitResponsesTools(raw)
	if len(functionTools) != 1 {
		t.Fatalf("function tools = %d, want 1 (%+v)", len(functionTools), functionTools)
	}
	if functionTools[0].Type != "function" || functionTools[0].Function.Name != "lookup" {
		t.Fatalf("kept tool = %+v, want function lookup", functionTools[0])
	}
	if strings.Join(droppedTypes, ",") != "web_search,custom" {
		t.Fatalf("dropped types = %v, want [web_search custom]", droppedTypes)
	}
}

func TestSplitResponsesToolsKeepsFlattenedFunction(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`[{"type":"function","name":"ReadFile","parameters":{"type":"object"}}]`)
	functionTools, droppedTypes := SplitResponsesTools(raw)
	if len(functionTools) != 1 || functionTools[0].Function.Name != "ReadFile" {
		t.Fatalf("function tools = %+v, want ReadFile", functionTools)
	}
	if len(droppedTypes) != 0 {
		t.Fatalf("dropped types = %v, want none", droppedTypes)
	}
}

func TestSplitResponsesToolsKeepsTopLevelNameSchema(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`[{"name":"weather","input_schema":{"type":"object"}}]`)
	functionTools, droppedTypes := SplitResponsesTools(raw)
	if len(functionTools) != 1 || functionTools[0].Function.Name != "weather" {
		t.Fatalf("function tools = %+v, want weather", functionTools)
	}
	if len(droppedTypes) != 0 {
		t.Fatalf("dropped types = %v, want none", droppedTypes)
	}
}

func TestSplitResponsesToolsEmptyAndNil(t *testing.T) {
	t.Parallel()

	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("[]"), json.RawMessage("null")} {
		functionTools, droppedTypes := SplitResponsesTools(raw)
		if functionTools != nil || droppedTypes != nil {
			t.Fatalf("raw %q -> (%v, %v), want (nil, nil)", string(raw), functionTools, droppedTypes)
		}
	}
}

func TestSplitResponsesToolsMalformedEntry(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`["not an object"]`)
	functionTools, droppedTypes := SplitResponsesTools(raw)
	if len(functionTools) != 0 {
		t.Fatalf("function tools = %+v, want none", functionTools)
	}
	if strings.Join(droppedTypes, ",") != "malformed" {
		t.Fatalf("dropped types = %v, want [malformed]", droppedTypes)
	}
}

func TestSplitResponsesToolsUnnamedEntry(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`[{"parameters":{"type":"object"}}]`)
	functionTools, droppedTypes := SplitResponsesTools(raw)
	if len(functionTools) != 0 {
		t.Fatalf("function tools = %+v, want none", functionTools)
	}
	if strings.Join(droppedTypes, ",") != "unnamed" {
		t.Fatalf("dropped types = %v, want [unnamed]", droppedTypes)
	}
}
