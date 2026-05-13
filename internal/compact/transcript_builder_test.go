package compact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"goodkind.io/clyde/internal/contextcount"
)

type transcriptBuilderEntry struct {
	UUID        string                    `json:"uuid,omitempty"`
	ParentUUID  string                    `json:"parentUuid,omitempty"`
	IsSidechain bool                      `json:"isSidechain,omitempty"`
	Type        string                    `json:"type"`
	Subtype     string                    `json:"subtype,omitempty"`
	Timestamp   string                    `json:"timestamp,omitempty"`
	Message     *transcriptBuilderMessage `json:"message,omitempty"`
	CWD         string                    `json:"cwd,omitempty"`
	SessionID   string                    `json:"sessionId,omitempty"`
}

type transcriptBuilderMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func TestBuildTranscriptPreservesMessagesAndFiltersSidechain(t *testing.T) {
	homeDir := t.TempDir()
	workspaceRoot := t.TempDir()
	t.Setenv("HOME", homeDir)

	slice := loadTranscriptBuilderSlice(t, []transcriptBuilderEntry{
		{
			UUID:      "system-1",
			Type:      "system",
			Timestamp: "2026-05-13T12:00:00Z",
			CWD:       workspaceRoot,
			SessionID: "session-1",
		},
		{
			UUID:      "user-1",
			Type:      "user",
			Timestamp: "2026-05-13T12:00:01Z",
			Message: messageForRawContent(
				"user",
				json.RawMessage(`"plain user text"`),
			),
		},
		{
			UUID:      "assistant-1",
			Type:      "assistant",
			Timestamp: "2026-05-13T12:00:02Z",
			Message: messageForRawContent(
				"assistant",
				json.RawMessage(
					`[`+
						`{"type":"text","text":"visible text"},`+
						`{"type":"thinking","thinking":"private thought","signature":"sig-1"},`+
						`{"type":"redacted_thinking","data":"opaque-redacted-data"},`+
						`{"type":"tool_use","id":"toolu_string","name":"Read","input":{"path":"README.md","offset":10}},`+
						`{"type":"tool_use","id":"toolu_list","name":"Bash","input":{"command":"pwd"}},`+
						`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}`+
						`]`,
				),
			),
		},
		{
			UUID:      "user-2",
			Type:      "user",
			Timestamp: "2026-05-13T12:00:03Z",
			Message: messageForRawContent(
				"user",
				json.RawMessage(
					`[`+
						`{"type":"tool_result","tool_use_id":"toolu_string","content":"string result\nline two","is_error":true}`+
						`]`,
				),
			),
		},
		{
			UUID:        "sidechain-1",
			IsSidechain: true,
			Type:        "user",
			Timestamp:   "2026-05-13T12:00:04Z",
			Message: messageForRawContent(
				"user",
				json.RawMessage(`"hidden sidechain text"`),
			),
		},
		{
			UUID:      "user-3",
			Type:      "user",
			Timestamp: "2026-05-13T12:00:05Z",
			Message: messageForRawContent(
				"user",
				json.RawMessage(
					`[`+
						`{"type":"tool_result","tool_use_id":"toolu_list","content":[`+
						`{"type":"text","text":"listed text"},`+
						`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/4AAQSkZJRg=="}}`+
						`],"is_error":false}`+
						`]`,
				),
			),
		},
	})

	got, err := BuildTranscript(slice, RuntimeUpfront{Model: "claude-sonnet-4-5"}, nil, nil)
	if err != nil {
		t.Fatalf("BuildTranscript returned error: %v", err)
	}

	wantMessages := []contextcount.Message{
		{
			Role: "user",
			Content: []contextcount.ContentBlock{
				{Type: "text", Text: "plain user text"},
			},
		},
		{
			Role: "assistant",
			Content: []contextcount.ContentBlock{
				{Type: "text", Text: "visible text"},
				{Type: "thinking", Thinking: "private thought", Signature: "sig-1"},
				{Type: "redacted_thinking", RedactedData: "opaque-redacted-data"},
				{
					Type:      "tool_use",
					ToolUseID: "toolu_string",
					ToolName:  "Read",
					ToolInput: json.RawMessage(`{"path":"README.md","offset":10}`),
				},
				{
					Type:      "tool_use",
					ToolUseID: "toolu_list",
					ToolName:  "Bash",
					ToolInput: json.RawMessage(`{"command":"pwd"}`),
				},
				{
					Type: "image",
					ImageSource: contextcount.ImageSource{
						Type:      "base64",
						MediaType: "image/png",
						Data:      "iVBORw0KGgo=",
					},
				},
			},
		},
		{
			Role: "user",
			Content: []contextcount.ContentBlock{
				{
					Type:           "tool_result",
					ToolUseID:      "toolu_string",
					ToolResultText: "string result\nline two",
					IsError:        true,
				},
			},
		},
		{
			Role: "user",
			Content: []contextcount.ContentBlock{
				{
					Type:      "tool_result",
					ToolUseID: "toolu_list",
					ToolResult: []contextcount.ContentBlock{
						{Type: "text", Text: "listed text"},
						{
							Type: "image",
							ImageSource: contextcount.ImageSource{
								Type:      "base64",
								MediaType: "image/jpeg",
								Data:      "/9j/4AAQSkZJRg==",
							},
						},
					},
				},
			},
		},
	}

	if got.Model != "claude-sonnet-4-5" {
		t.Fatalf("Model = %q, want claude-sonnet-4-5", got.Model)
	}
	if len(got.System) != 0 {
		t.Fatalf("System length = %d, want 0", len(got.System))
	}
	if len(got.Tools) != 0 {
		t.Fatalf("Tools length = %d, want 0", len(got.Tools))
	}
	if !reflect.DeepEqual(got.Messages, wantMessages) {
		t.Fatalf("Messages mismatch\ngot:  %#v\nwant: %#v", got.Messages, wantMessages)
	}
	assertRawMessageBytes(t, got.Messages[1].Content[3].ToolInput, `{"path":"README.md","offset":10}`)
	assertRawMessageBytes(t, got.Messages[1].Content[4].ToolInput, `{"command":"pwd"}`)
}

func TestBuildTranscriptUsesPostBoundaryMessages(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	slice := loadTranscriptBuilderSlice(t, []transcriptBuilderEntry{
		{
			UUID:      "pre-user",
			Type:      "user",
			Timestamp: "2026-05-13T12:00:00Z",
			Message:   messageForRawContent("user", json.RawMessage(`"pre-boundary text"`)),
		},
		{
			UUID:      "boundary",
			Type:      "system",
			Subtype:   "compact_boundary",
			Timestamp: "2026-05-13T12:00:01Z",
		},
		{
			UUID:      "post-user",
			Type:      "user",
			Timestamp: "2026-05-13T12:00:02Z",
			Message:   messageForRawContent("user", json.RawMessage(`"post-boundary text"`)),
		},
	})

	got, err := BuildTranscript(slice, RuntimeUpfront{Model: "claude-sonnet-4-5"}, nil, nil)
	if err != nil {
		t.Fatalf("BuildTranscript returned error: %v", err)
	}

	wantMessages := []contextcount.Message{
		{
			Role: "user",
			Content: []contextcount.ContentBlock{
				{Type: "text", Text: "post-boundary text"},
			},
		},
	}
	if !reflect.DeepEqual(got.Messages, wantMessages) {
		t.Fatalf("Messages mismatch\ngot:  %#v\nwant: %#v", got.Messages, wantMessages)
	}
}

func TestBuildTranscriptCapturesSystemToolsMemoryAndSkills(t *testing.T) {
	homeDir := t.TempDir()
	workspaceRoot := t.TempDir()
	t.Setenv("HOME", homeDir)

	writeBuilderFile(t, filepath.Join(homeDir, ".claude", "CLAUDE.md"), "user memory\n")
	writeBuilderFile(t, filepath.Join(workspaceRoot, "CLAUDE.md"), "project memory\n")
	writeBuilderFile(
		t,
		filepath.Join(
			homeDir,
			".claude",
			"projects",
			encodeClaudeProjectPath(workspaceRoot),
			"memory",
			"MEMORY.md",
		),
		"auto memory\n",
	)
	writeBuilderFile(t, filepath.Join(homeDir, ".claude", "skills", "read", "SKILL.md"), "user skill\n")
	writeBuilderFile(
		t,
		filepath.Join(workspaceRoot, ".claude", "skills", "write", "SKILL.md"),
		"project skill\n",
	)

	slice := loadTranscriptBuilderSlice(t, []transcriptBuilderEntry{
		{
			UUID:      "system-1",
			Type:      "system",
			Timestamp: "2026-05-13T12:00:00Z",
			CWD:       workspaceRoot,
			SessionID: "session-1",
		},
		{
			UUID:      "user-1",
			Type:      "user",
			Timestamp: "2026-05-13T12:00:01Z",
			Message:   messageForRawContent("user", json.RawMessage(`"hello"`)),
		},
	})
	capturedSystem := []byte(`{"system":[{"type":"text","text":"captured system"}]}`)
	capturedTools := []byte(
		`{"tools":[{"name":"Read","description":"read files","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`,
	)

	got, err := BuildTranscript(
		slice,
		RuntimeUpfront{Model: "claude-opus-4-1m"},
		capturedSystem,
		capturedTools,
	)
	if err != nil {
		t.Fatalf("BuildTranscript returned error: %v", err)
	}

	wantSystemTexts := []string{
		"captured system",
		"user memory\n",
		"project memory\n",
		"auto memory\n",
		"user skill\n",
		"project skill\n",
	}
	if gotSystemTexts := systemTexts(got.System); !reflect.DeepEqual(gotSystemTexts, wantSystemTexts) {
		t.Fatalf("System texts mismatch\ngot:  %#v\nwant: %#v", gotSystemTexts, wantSystemTexts)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("Tools length = %d, want 1", len(got.Tools))
	}
	if got.Tools[0].Name != "Read" {
		t.Fatalf("Tools[0].Name = %q, want Read", got.Tools[0].Name)
	}
	if got.Tools[0].Description != "read files" {
		t.Fatalf("Tools[0].Description = %q, want read files", got.Tools[0].Description)
	}
	assertRawMessageBytes(
		t,
		got.Tools[0].InputSchema,
		`{"type":"object","properties":{"path":{"type":"string"}}}`,
	)
}

func TestBuildTranscriptEmptyCapturesLeaveSystemAndToolsEmpty(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	slice := loadTranscriptBuilderSlice(t, []transcriptBuilderEntry{
		{
			UUID:      "user-1",
			Type:      "user",
			Timestamp: "2026-05-13T12:00:00Z",
			Message:   messageForRawContent("user", json.RawMessage(`"hello"`)),
		},
	})

	got, err := BuildTranscript(
		slice,
		RuntimeUpfront{Model: "claude-sonnet-4-5"},
		[]byte("   "),
		[]byte(""),
	)
	if err != nil {
		t.Fatalf("BuildTranscript returned error: %v", err)
	}
	if len(got.System) != 0 {
		t.Fatalf("System length = %d, want 0", len(got.System))
	}
	if len(got.Tools) != 0 {
		t.Fatalf("Tools length = %d, want 0", len(got.Tools))
	}
}

func loadTranscriptBuilderSlice(t *testing.T, entries []transcriptBuilderEntry) *Slice {
	t.Helper()
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal transcript entry: %v", err)
		}
		lines = append(lines, string(encoded))
	}
	path := writeTranscript(t, lines)
	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice returned error: %v", err)
	}
	return slice
}

func messageForRawContent(role string, content json.RawMessage) *transcriptBuilderMessage {
	return &transcriptBuilderMessage{Role: role, Content: content}
}

func writeBuilderFile(t *testing.T, path string, content string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func systemTexts(blocks []contextcount.TextBlock) []string {
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		texts = append(texts, block.Text)
	}
	return texts
}

func assertRawMessageBytes(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	if string(got) != want {
		t.Fatalf("raw message = %s, want %s", string(got), want)
	}
}
