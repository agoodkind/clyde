package cursorjsonl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamMessagesYieldsMessagesInOrderAndSkipsEventsAndMalformedLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	lines := []string{
		`{"type":"turn_ended","status":"success"}`,
		`not-json`,
		`{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"read_file","input":{"path":"/tmp/example","limit":2}},{"type":"unknown","text":"ignored"}]}}`,
	}
	writeTestFile(t, path, strings.Join(lines, "\n")+"\n")

	messages, err := ReadMessages(path)
	if err != nil {
		t.Fatalf("ReadMessages returned error: %v", err)
	}

	if len(messages) != 2 {
		t.Fatalf("ReadMessages returned %d messages, want 2: %#v", len(messages), messages)
	}
	if messages[0].Role != RoleUser {
		t.Fatalf("first role = %q, want %q", messages[0].Role, RoleUser)
	}
	if len(messages[0].Parts) != 1 || messages[0].Parts[0].Text != "hello" {
		t.Fatalf("first parts = %#v, want one hello text part", messages[0].Parts)
	}
	if messages[1].Role != RoleAssistant {
		t.Fatalf("second role = %q, want %q", messages[1].Role, RoleAssistant)
	}
	if len(messages[1].Parts) != 2 {
		t.Fatalf("second message has %d parts, want 2: %#v", len(messages[1].Parts), messages[1].Parts)
	}
	if messages[1].Parts[0].Type != PartTypeText || messages[1].Parts[0].Text != "hi" {
		t.Fatalf("assistant first part = %#v, want hi text part", messages[1].Parts[0])
	}
	toolPart := messages[1].Parts[1]
	if toolPart.Type != PartTypeToolUse {
		t.Fatalf("tool part type = %q, want %q", toolPart.Type, PartTypeToolUse)
	}
	if toolPart.ToolName != "read_file" {
		t.Fatalf("ToolName = %q, want read_file", toolPart.ToolName)
	}
	var toolInput struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(toolPart.ToolInput, &toolInput); err != nil {
		t.Fatalf("tool input did not preserve raw JSON object: %v", err)
	}
	if toolInput.Path != "/tmp/example" || toolInput.Limit != 2 {
		t.Fatalf("tool input = %#v, want path and limit preserved", toolInput)
	}
}

func TestStreamMessagesParsesLineLongerThanScannerDefault(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	longValue := strings.Repeat("x", 70*1024)
	line := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"write_file","input":{"content":"` + longValue + `"}}]}}`
	writeTestFile(t, path, line+"\n")

	messages, err := ReadMessages(path)
	if err != nil {
		t.Fatalf("ReadMessages returned error: %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("ReadMessages returned %d messages, want 1", len(messages))
	}
	var toolInput struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(messages[0].Parts[0].ToolInput, &toolInput); err != nil {
		t.Fatalf("tool input did not preserve long raw JSON object: %v", err)
	}
	if toolInput.Content != longValue {
		t.Fatalf("long tool input length = %d, want %d", len(toolInput.Content), len(longValue))
	}
}

func TestScanHeaderReturnsFirstUserTextAndHasMessages(t *testing.T) {
	t.Parallel()

	transcriptPath := filepath.Join(t.TempDir(), testConversationID, testConversationID+".jsonl")
	writeTestFile(t, transcriptPath, strings.Join([]string{
		`{"type":"turn_ended","status":"error","error":"failed"}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"assistant first"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"first user title"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"second user"}]}}`,
	}, "\n")+"\n")

	header, err := ScanHeader(transcriptPath)
	if err != nil {
		t.Fatalf("ScanHeader returned error: %v", err)
	}

	if header.ConversationID != testConversationID {
		t.Fatalf("ConversationID = %q, want %q", header.ConversationID, testConversationID)
	}
	if !header.HasMessages {
		t.Fatal("HasMessages = false, want true")
	}
	if header.FirstUserText != "first user title" {
		t.Fatalf("FirstUserText = %q, want first user title", header.FirstUserText)
	}
}

func TestScanHeaderReturnsNoMessagesForEventsOnlyFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, `{"type":"turn_ended","status":"success"}`+"\n")

	header, err := ScanHeader(path)
	if err != nil {
		t.Fatalf("ScanHeader returned error: %v", err)
	}

	if header.HasMessages {
		t.Fatal("HasMessages = true, want false")
	}
	if header.FirstUserText != "" {
		t.Fatalf("FirstUserText = %q, want empty", header.FirstUserText)
	}
}

func TestStreamMessagesReturnsCallbackError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), testConversationID+".jsonl")
	writeTestFile(t, path, `{"role":"user","message":{"content":[{"type":"text","text":"hello"}]}}`+"\n")
	callbackErr := os.ErrPermission

	err := StreamMessages(path, func(_ TranscriptMessage) error {
		return callbackErr
	})
	if err == nil {
		t.Fatal("StreamMessages returned nil error, want callback error")
	}
}
