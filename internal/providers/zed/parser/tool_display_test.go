package parser

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestToolDisplayText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		wantText string
		wantLang string
	}{
		{name: "command", input: `{"command":"git status"}`, wantText: "git status", wantLang: "bash"},
		{name: "path", input: `{"path":"/repo/main.go"}`, wantText: "/repo/main.go", wantLang: ""},
		{name: "regex", input: `{"regex":"TODO"}`, wantText: "TODO", wantLang: ""},
		{name: "query", input: `{"query":"adapter error"}`, wantText: "adapter error", wantLang: ""},
		{name: "unknown", input: `{"other":"value"}`, wantText: "", wantLang: ""},
		{name: "empty", input: "", wantText: "", wantLang: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			display, language := toolDisplayText("tool", transcript.ToolInputJSON{Raw: json.RawMessage(test.input)})
			if display != test.wantText || language != test.wantLang {
				t.Fatalf("toolDisplayText() = %q, %q; want %q, %q", display, language, test.wantText, test.wantLang)
			}
		})
	}
}

func TestZedFixtureFillsToolDisplay(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Tool displays",
		"updated_at":"2026-07-30T12:00:00Z",
		"messages":[
			{"Agent":{"content":[
				{"ToolUse":{"id":"tool-command","name":"terminal","input":{"command":"git status"}}},
				{"ToolUse":{"id":"tool-path","name":"read","input":{"path":"/repo/main.go"}}},
				{"ToolUse":{"id":"tool-regex","name":"search","input":{"regex":"TODO"}}},
				{"ToolUse":{"id":"tool-query","name":"search","input":{"query":"adapter error"}}}
			],"tool_results":{}}}
		]
	}`)
	writeThreadsRow(t, root, "thread-tools", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-tools", "", "Tool displays", "", updatedAt)
	p := New()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("discover fixture: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	messages, err := conversation.CollectMessages(p.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("stream fixture: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	assertZedFixtureToolDisplay(t, messages[0].Tools, []string{
		"git status",
		"/repo/main.go",
		"TODO",
		"adapter error",
	})
}

func assertZedFixtureToolDisplay(t *testing.T, tools []transcript.ToolCall, want []string) {
	t.Helper()
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	withInput := 0
	withDisplay := 0
	for i, tool := range tools {
		if tool.Input.Len() > 0 {
			withInput++
			if tool.Display != "" {
				withDisplay++
			}
		}
		if tool.Display != want[i] {
			t.Fatalf("tool %d display = %q, want %q", i, tool.Display, want[i])
		}
	}
	if withDisplay*100 < withInput*90 {
		t.Fatalf("fixture display coverage = %d/%d, want at least 90%%", withDisplay, withInput)
	}
}
