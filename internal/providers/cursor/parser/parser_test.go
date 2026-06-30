package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

func TestCursorParserStreamsTextAndToolUse(t *testing.T) {
	t.Parallel()

	path := writeCursorTranscript(t, `{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","id":"tool-1","name":"read_file","input":{"path":"README.md"}}]}}
`)
	parser := New()
	messages, err := conversation.CollectMessages(parser.Stream(path, conversation.LoadOptions{}))
	if err != nil {
		t.Fatalf("CollectMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Text != "hello" {
		t.Fatalf("first message = %#v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Text != "working" {
		t.Fatalf("second message text = %#v", messages[1])
	}
	if messages[1].Timestamp.IsZero() {
		t.Fatal("second message timestamp is zero")
	}
	if !messages[1].HasTools || len(messages[1].Tools) != 1 {
		t.Fatalf("second message tools = %#v", messages[1].Tools)
	}
	tool := messages[1].Tools[0]
	if tool.ID != "tool-1" || tool.Name != "read_file" {
		t.Fatalf("tool = %#v", tool)
	}
	if !strings.Contains(string(tool.Input.Raw), "README.md") {
		t.Fatalf("tool input raw = %s", string(tool.Input.Raw))
	}
}

func TestCursorParserMalformedJSONLIncludesPathContext(t *testing.T) {
	t.Parallel()

	path := writeCursorTranscript(t, `{"role":"user","message":{"content":[{"type":"text","text":"ok"}]}}
{not-json}
`)
	parser := New()
	_, err := conversation.CollectMessages(parser.Stream(path, conversation.LoadOptions{}))
	if err == nil {
		t.Fatal("CollectMessages returned nil error")
	}
	if !strings.Contains(err.Error(), path+":2") {
		t.Fatalf("error = %v, want path and line", err)
	}
}

func TestCursorParserHonorsIncludeSystemMessages(t *testing.T) {
	t.Parallel()

	path := writeCursorTranscript(t, `{"role":"system","message":{"content":[{"type":"text","text":"system setup"}]}}
{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}
`)
	parser := New()
	messages, err := conversation.CollectMessages(parser.Stream(path, conversation.LoadOptions{}))
	if err != nil {
		t.Fatalf("CollectMessages: %v", err)
	}
	if len(messages) != 1 || messages[0].Role != "user" {
		t.Fatalf("messages = %#v, want only user message", messages)
	}

	messages, err = conversation.CollectMessages(parser.Stream(path, conversation.LoadOptions{IncludeSystemMessages: true}))
	if err != nil {
		t.Fatalf("CollectMessages with systems: %v", err)
	}
	if len(messages) != 2 || messages[0].Role != "system" {
		t.Fatalf("messages = %#v, want system message included", messages)
	}
}

func TestCursorParserDiscoversAgentTranscriptRecords(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	transcriptPath := filepath.Join(homeDir, ".cursor", "projects", "Users-me-project", "agent-transcripts", "conversation-1", "conversation-1.jsonl")
	writeTestFile(t, transcriptPath, `{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}
`)

	parser := New()
	parser.HomeDir = homeDir
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	record, ok := parser.ScanRecord(candidates[0].Path, candidates[0].Stamp)
	if !ok {
		t.Fatal("ScanRecord returned ok=false")
	}
	if record.ID != "cursor:conversation-1" || record.NativeID != "conversation-1" {
		t.Fatalf("record identity = %#v", record)
	}
	wantWorkspace := filepath.Join(string(filepath.Separator), "Users", "me", "project")
	if record.WorkspaceRoot != wantWorkspace {
		t.Fatalf("workspace root = %q, want %q", record.WorkspaceRoot, wantWorkspace)
	}
}

func writeCursorTranscript(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	writeTestFile(t, path, body)
	return path
}

func writeTestFile(t *testing.T, path string, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
