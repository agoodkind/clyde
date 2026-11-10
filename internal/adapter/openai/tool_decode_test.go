package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolUnmarshalOpenAICanonical(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"zip":{"type":"string"}}},"strict":true}}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tool.Type != "function" {
		t.Fatalf("tool type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "weather" {
		t.Fatalf("name = %q, want weather", tool.Function.Name)
	}
	if tool.Function.Description != "Get weather" {
		t.Fatalf("description = %q, want Get weather", tool.Function.Description)
	}
	if tool.Function.Strict == nil || !*tool.Function.Strict {
		t.Fatalf("strict = %v, want true", tool.Function.Strict)
	}
	if strings.TrimSpace(string(tool.Function.Parameters)) != `{"type":"object","properties":{"zip":{"type":"string"}}}` {
		t.Fatalf("parameters = %q", tool.Function.Parameters)
	}
}

func TestToolUnmarshalAnthropicNative(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"zip":{"type":"string"}}}}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tool.Type != "function" {
		t.Fatalf("tool type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "weather" {
		t.Fatalf("name = %q, want weather", tool.Function.Name)
	}
	if tool.Function.Description != "Get weather" {
		t.Fatalf("description = %q, want Get weather", tool.Function.Description)
	}
	if strings.TrimSpace(string(tool.Function.Parameters)) != `{"type":"object","properties":{"zip":{"type":"string"}}}` {
		t.Fatalf("parameters = %q", tool.Function.Parameters)
	}
}

func TestToolUnmarshalCursorFlattenedFunction(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"function","name":"ReadFile","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]},"strict":false}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tool.Type != "function" {
		t.Fatalf("tool type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "ReadFile" {
		t.Fatalf("name = %q, want ReadFile", tool.Function.Name)
	}
	if tool.Function.Strict == nil || *tool.Function.Strict {
		t.Fatalf("strict = %v, want false", tool.Function.Strict)
	}
	if !strings.Contains(string(tool.Function.Parameters), `"required":["path"]`) {
		t.Fatalf("parameters lost required path schema: %s", tool.Function.Parameters)
	}
}

func TestToolUnmarshalAnthropicCustomType(t *testing.T) {
	t.Parallel()

	raw := json.RawMessage(`{"type":"custom","name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"zip":{"type":"string"}}}}`)
	var tool Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tool.Type != "function" {
		t.Fatalf("tool type = %q, want function", tool.Type)
	}
	if tool.Function.Name != "weather" {
		t.Fatalf("name = %q, want weather", tool.Function.Name)
	}
}

func TestToolUnmarshalRejectsEmptyObject(t *testing.T) {
	t.Parallel()

	var tool Tool
	if err := json.Unmarshal(json.RawMessage(`{}`), &tool); err == nil {
		t.Fatalf("unexpected success: %#v", tool)
	}
}

func TestToolUnmarshalRejectsUnknownType(t *testing.T) {
	t.Parallel()

	var tool Tool
	err := json.Unmarshal(
		json.RawMessage(`{"type":"web_search","name":"weather","description":"Get weather","input_schema":{"type":"object"}}`),
		&tool,
	)
	if err == nil {
		t.Fatalf("unexpected success: %#v", tool)
	}
}

func TestToolUnmarshalAnthropicRoundTripIntoOpenAI(t *testing.T) {
	t.Parallel()

	anthropicShape := json.RawMessage(`{"name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"zip":{"type":"string"}}}}`)
	var tool Tool
	if err := json.Unmarshal(anthropicShape, &tool); err != nil {
		t.Fatalf("unmarshal anthropic: %v", err)
	}

	canonical, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	var tt Tool
	if err := json.Unmarshal(canonical, &tt); err != nil {
		t.Fatalf("unmarshal OpenAI tool: %v", err)
	}
	if tt.Function.Name != "weather" {
		t.Fatalf("name = %q, want weather", tt.Function.Name)
	}
	if strings.TrimSpace(string(tt.Function.Parameters)) != `{"type":"object","properties":{"zip":{"type":"string"}}}` {
		t.Fatalf("parameters = %q", tt.Function.Parameters)
	}
}
