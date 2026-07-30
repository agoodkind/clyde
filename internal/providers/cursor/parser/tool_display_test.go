package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
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
		{name: "cmd", input: `{"cmd":"pwd"}`, wantText: "pwd", wantLang: "bash"},
		{name: "relative workspace path", input: `{"relative_workspace_path":"main.go"}`, wantText: "main.go", wantLang: ""},
		{name: "target file", input: `{"target_file":"README.md"}`, wantText: "README.md", wantLang: ""},
		{name: "path", input: `{"path":"/repo/main.go"}`, wantText: "/repo/main.go", wantLang: ""},
		{name: "prompt", input: `{"prompt":"inspect callers"}`, wantText: "inspect callers", wantLang: ""},
		{name: "description", input: `{"description":"inspect callers"}`, wantText: "inspect callers", wantLang: ""},
		{name: "query", input: `{"query":"adapter error"}`, wantText: "adapter error", wantLang: ""},
		{name: "pattern", input: `{"pattern":"TODO"}`, wantText: "TODO", wantLang: ""},
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

func TestCursorFixtureFillsToolDisplay(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	body := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"run_command","input":{"command":"git status"}},{"type":"tool_use","name":"run_command","input":{"cmd":"pwd"}},{"type":"tool_use","name":"read_file","input":{"relative_workspace_path":"main.go"}},{"type":"tool_use","name":"read_file","input":{"target_file":"README.md"}},{"type":"tool_use","name":"read_file","input":{"path":"/repo/main.go"}},{"type":"tool_use","name":"Task","input":{"prompt":"inspect callers"}},{"type":"tool_use","name":"Task","input":{"description":"inspect tool shapes"}},{"type":"tool_use","name":"search","input":{"query":"adapter error"}},{"type":"tool_use","name":"search","input":{"pattern":"TODO"}}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	var mapped transcript.Message
	if err := cursorjsonl.StreamMessages(path, func(decoded cursorjsonl.TranscriptMessage) error {
		message, include := mapJSONLMessage(decoded, conversation.LoadOptions{
			IncludeSystemPrompts:  false,
			IncludeSystemMessages: false,
			IncludeToolOutputs:    false,
		})
		if !include {
			t.Fatal("mapJSONLMessage include = false, want true")
		}
		mapped = message
		return nil
	}); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	assertCursorFixtureToolDisplay(t, mapped.Tools, []string{
		"git status",
		"pwd",
		"main.go",
		"README.md",
		"/repo/main.go",
		"inspect callers",
		"inspect tool shapes",
		"adapter error",
		"TODO",
	})
}

func TestMapComposerBubbleFillsToolDisplay(t *testing.T) {
	t.Parallel()
	message, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-tool",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          "",
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall: &cursorstore.BubbleToolCall{
			Name:    "run_command",
			RawArgs: `{"cmd":"pwd"}`,
			Result:  "",
			Status:  "success",
		},
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("mapComposerBubble include = false, want true")
	}
	if len(message.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(message.Tools))
	}
	if message.Tools[0].Display != "pwd" || message.Tools[0].DisplayLang != "bash" {
		t.Fatalf("tool display = %q, %q; want pwd, bash", message.Tools[0].Display, message.Tools[0].DisplayLang)
	}
}

func assertCursorFixtureToolDisplay(t *testing.T, tools []transcript.ToolCall, want []string) {
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
