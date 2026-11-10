package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSummarizeChatBody(t *testing.T) {
	raw := []byte(`{
		"model": "clyde-haiku-4-5",
		"stream": true,
		"messages": [
			{"role":"system","content":"` + strings.Repeat("x", 2400) + `"},
			{"role":"user","content":"alpha"},
			{"role":"assistant","tool_calls":[{"id":"tool-call-1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"NYC\",\"path\":\"/tmp/weather.json\"}"}}]},
			{"role":"assistant","content":"beta"},
			{"role":"user","content":[{"type":"text","text":"gamma"}]},
			{"role":"assistant","content":"delta"},
			{"role":"user","content":"epsilon"}
		],
		"tools": [
			{"type":"function","function":{"name":"tool-a","description":"desc","parameters":{"a":1}}},
			{"type":"function","function":{"name":"tool-b","description":"desc","parameters":{"b":2}}}
		],
		"functions": [{"name":"fn-a","description":"legacy", "parameters":{"c":3}}],
		"tool_choice": "auto"
	}`)

	summary, err := SummarizeChatBody(raw)
	if err != nil {
		t.Fatalf("SummarizeChatBody: %v", err)
	}

	if summary.Model != "clyde-haiku-4-5" {
		t.Fatalf("Model = %q", summary.Model)
	}
	if summary.Stream != true {
		t.Fatalf("Stream = %v", summary.Stream)
	}
	if summary.MessageCount != 7 {
		t.Fatalf("MessageCount = %d", summary.MessageCount)
	}
	if len(summary.Messages) != 6 {
		t.Fatalf("len(Messages) = %d", len(summary.Messages))
	}
	if summary.Messages[0].Role != "system" {
		t.Fatalf("first sampled role = %q", summary.Messages[0].Role)
	}
	if summary.Messages[0].ContentChars != 2048 {
		t.Fatalf("first message ContentChars = %d", summary.Messages[0].ContentChars)
	}
	if summary.Messages[2].HasToolCalls != true {
		t.Fatalf("expected tool call sample message")
	}
	if summary.Messages[2].ToolCallCount != 1 {
		t.Fatalf("ToolCallCount = %d", summary.Messages[2].ToolCallCount)
	}
	if summary.Messages[2].ToolCallID != "tool-call-1" {
		t.Fatalf("ToolCallID = %q", summary.Messages[2].ToolCallID)
	}
	if got := summary.Messages[2].ToolCallIDs; len(got) != 1 || got[0] != "tool-call-1" {
		t.Fatalf("ToolCallIDs = %v", got)
	}
	if got := summary.Messages[2].ToolCallNames; len(got) != 1 || got[0] != "weather" {
		t.Fatalf("ToolCallNames = %v", got)
	}
	if got := strings.Join(summary.Messages[2].ToolCallArgKeys, ","); got != "city,path" {
		t.Fatalf("ToolCallArgKeys = %q", got)
	}
	if got := summary.Messages[2].ToolCallPaths; len(got) != 1 || got[0] != "/tmp/weather.json" {
		t.Fatalf("ToolCallPaths = %v", got)
	}
	if summary.Messages[2].ToolCallArgChars == 0 {
		t.Fatalf("ToolCallArgChars = %d", summary.Messages[2].ToolCallArgChars)
	}
	if summary.ToolCount != 3 {
		t.Fatalf("ToolCount = %d", summary.ToolCount)
	}
	if len(summary.Tools) != 3 {
		t.Fatalf("len(Tools) = %d", len(summary.Tools))
	}
	if summary.Tools[0].Name != "tool-a" || summary.Tools[0].ParamsChars == 0 {
		t.Fatalf("first tool summary = %+v", summary.Tools[0])
	}
	if string(summary.ToolChoice) != "\"auto\"" {
		t.Fatalf("ToolChoice = %q", summary.ToolChoice)
	}

	var toolChoice string
	if err := json.Unmarshal(summary.ToolChoice, &toolChoice); err != nil {
		t.Fatalf("tool choice: %v", err)
	}
	if toolChoice != "auto" {
		t.Fatalf("tool_choice = %q", toolChoice)
	}
}

func TestSummarizeChatRequestNormalizesNullToolChoice(t *testing.T) {
	summary := SummarizeChatRequest(ChatRequest{
		Model:      "clyde-haiku-4-5",
		ToolChoice: json.RawMessage("null"),
	})
	if summary.ToolChoice != nil {
		t.Fatalf("ToolChoice should be nil when null, got %q", string(summary.ToolChoice))
	}
}
