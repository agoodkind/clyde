package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
		{name: "file path", input: `{"file_path":"/repo/main.go"}`, wantText: "/repo/main.go", wantLang: ""},
		{name: "pattern", input: `{"pattern":"TODO"}`, wantText: "TODO", wantLang: ""},
		{name: "prompt", input: `{"prompt":"inspect callers"}`, wantText: "inspect callers", wantLang: ""},
		{name: "url", input: `{"url":"https://example.com"}`, wantText: "https://example.com", wantLang: ""},
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

func TestClaudeFixtureFillsToolDisplay(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"uuid":"assistant-1","type":"assistant","timestamp":"2026-07-30T12:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tool-command","name":"Bash","input":{"command":"git status"}},{"type":"tool_use","id":"tool-path","name":"Read","input":{"file_path":"/repo/main.go"}},{"type":"tool_use","id":"tool-pattern","name":"Grep","input":{"pattern":"TODO"}},{"type":"tool_use","id":"tool-prompt","name":"Task","input":{"prompt":"inspect callers"}},{"type":"tool_use","id":"tool-url","name":"WebFetch","input":{"url":"https://example.com"}},{"type":"tool_use","id":"tool-query","name":"Search","input":{"query":"adapter error"}}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
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
	assertClaudeFixtureToolDisplay(t, messages[0].Tools, []string{
		"git status",
		"/repo/main.go",
		"TODO",
		"inspect callers",
		"https://example.com",
		"adapter error",
	})
}

func assertClaudeFixtureToolDisplay(t *testing.T, tools []transcript.ToolCall, want []string) {
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
