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

// TestAPlanAndAQuestionAreStored covers the two shapes whose text exists only
// inside the tool call. Every ExitPlanMode call sampled from real transcripts
// carried its plan with no assistant text beside it and no other copy in the
// file, so a renderer that skipped the plan would put an approved document
// beyond reach of any search.
func TestAPlanAndAQuestionAreStored(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		input    string
		want     string
		wantLang string
	}{
		{
			name:     "a plan is stored as the markdown it was written in",
			input:    `{"plan":"# Fix CI\n\nTwo repos have red CI."}`,
			want:     "# Fix CI\n\nTwo repos have red CI.",
			wantLang: "markdown",
		},
		{
			name:     "a question is stored with the choices offered under it",
			input:    `{"questions":[{"question":"Which split?","options":[{"label":"Broadest","description":"one PR"},{"label":"Stub","description":"two PRs"}]}]}`,
			want:     "Which split?\nBroadest\none PR\nStub\ntwo PRs",
			wantLang: "",
		},
		{
			name:     "a file edit still stores its path alone",
			input:    `{"file_path":"/tmp/a.go","old_string":"x","new_string":"y"}`,
			want:     "/tmp/a.go",
			wantLang: "",
		},
		{
			name:     "a shell command still wins over everything else",
			input:    `{"command":"ls -la","plan":"ignored"}`,
			want:     "ls -la",
			wantLang: "bash",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, gotLang := toolDisplayText("", transcript.ToolInputJSON{Raw: []byte(testCase.input)})
			if got != testCase.want {
				t.Fatalf("display = %q, want %q", got, testCase.want)
			}
			if gotLang != testCase.wantLang {
				t.Fatalf("language = %q, want %q", gotLang, testCase.wantLang)
			}
		})
	}
}

// TestAChoiceWrittenAsAStringStillCarriesTheQuestion covers the shape that made
// eight rows in a live corpus hold nothing but the tool's name. One tool writes
// each choice as an object with a label and a description, another writes it as
// a bare string, and a decode that insisted on the object shape failed the whole
// payload and took the question down with it.
func TestAChoiceWrittenAsAStringStillCarriesTheQuestion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "choices written as bare strings",
			input: `{"questions":[{"question":"Which approach?","options":["Script belt","Simple belt"]}]}`,
			want:  "Which approach?\nScript belt\nSimple belt",
		},
		{
			name:  "choices written as objects",
			input: `{"questions":[{"question":"Which approach?","options":[{"label":"Script belt","description":"lowest false positives"}]}]}`,
			want:  "Which approach?\nScript belt\nlowest false positives",
		},
		{
			name:  "a question with no choices at all",
			input: `{"questions":[{"question":"Proceed?"}]}`,
			want:  "Proceed?",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, _ := toolDisplayText("", transcript.ToolInputJSON{Raw: []byte(testCase.input)})
			if got != testCase.want {
				t.Fatalf("display = %q, want %q", got, testCase.want)
			}
		})
	}
}
