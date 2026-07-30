package codexstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
		{name: "command argv", input: `{"command":["git","status"]}`, wantText: "git status", wantLang: "bash"},
		{name: "command string", input: `{"command":"pwd"}`, wantText: "pwd", wantLang: "bash"},
		{name: "cmd", input: `{"cmd":"ls"}`, wantText: "ls", wantLang: "bash"},
		{name: "path", input: `{"path":"/repo/main.go"}`, wantText: "/repo/main.go", wantLang: ""},
		{name: "pattern", input: `{"pattern":"TODO"}`, wantText: "TODO", wantLang: ""},
		{name: "query", input: `{"query":"adapter error"}`, wantText: "adapter error", wantLang: ""},
		{name: "raw input", input: `"*** Begin Patch"`, wantText: "*** Begin Patch", wantLang: ""},
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

func TestCodexFixtureFillsToolDisplay(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	body := `{"timestamp":"2026-07-30T12:00:00Z","type":"response_item","payload":{"type":"function_call","call_id":"tool-command-array","name":"exec_command","arguments":"{\"command\":[\"git\",\"status\"]}"}}
{"timestamp":"2026-07-30T12:00:01Z","type":"response_item","payload":{"type":"function_call","call_id":"tool-command-string","name":"exec_command","arguments":"{\"command\":\"pwd\"}"}}
{"timestamp":"2026-07-30T12:00:02Z","type":"response_item","payload":{"type":"function_call","call_id":"tool-cmd","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}
{"timestamp":"2026-07-30T12:00:03Z","type":"response_item","payload":{"type":"function_call","call_id":"tool-path","name":"Read","arguments":"{\"path\":\"/repo/main.go\"}"}}
{"timestamp":"2026-07-30T12:00:04Z","type":"response_item","payload":{"type":"function_call","call_id":"tool-pattern","name":"Grep","arguments":"{\"pattern\":\"TODO\"}"}}
{"timestamp":"2026-07-30T12:00:05Z","type":"response_item","payload":{"type":"function_call","call_id":"tool-query","name":"Search","arguments":"{\"query\":\"adapter error\"}"}}
{"timestamp":"2026-07-30T12:00:06Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"tool-patch","name":"apply_patch","input":"*** Begin Patch"}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tools := make([]transcript.ToolCall, 0)
	for message, err := range StreamMessages(path, HistoryOptions{
		IncludeSystemMessages: false,
		IncludeSystemPrompts:  false,
	}) {
		if err != nil {
			t.Fatalf("stream fixture: %v", err)
		}
		tools = append(tools, message.Tools...)
	}
	assertCodexFixtureToolDisplay(t, tools, []string{
		"git status",
		"pwd",
		"ls",
		"/repo/main.go",
		"TODO",
		"adapter error",
		"*** Begin Patch",
	})
}

func assertCodexFixtureToolDisplay(t *testing.T, tools []transcript.ToolCall, want []string) {
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
