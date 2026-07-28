package parser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

const (
	turnsConversationID = "66666666-6666-4666-8666-666666666666"
	cleanConversationID = "77777777-7777-4777-8777-777777777777"
)

// streamJSONLTurns writes a Cursor CLI transcript and reads it back through the
// parser's public Stream boundary, which is the path every consumer uses.
func streamJSONLTurns(t *testing.T, lines []string) []conversationMessage {
	t.Helper()

	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", t.TempDir())
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	path := writeCursorJSONLTranscript(
		t,
		projectsDir,
		"Users-alice-source-cursor-repo",
		turnsConversationID,
		lines,
	)

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("CollectMessages returned error: %v", err)
	}

	out := make([]conversationMessage, 0, len(messages))
	for _, message := range messages {
		toolNames := make([]string, 0, len(message.Tools))
		for _, tool := range message.Tools {
			toolNames = append(toolNames, tool.Name)
		}
		out = append(out, conversationMessage{
			Role:      message.Role,
			Text:      message.Text,
			ToolNames: toolNames,
		})
	}
	return out
}

// conversationMessage is the narrow projection these tests assert on.
type conversationMessage struct {
	Role      string
	Text      string
	ToolNames []string
}

// TestStreamJoinsMultiRecordAssistantTurnIntoOneMessage is the core defect.
// Cursor writes one assistant turn across many consecutive lines, and every
// consumer saw one fragment per line where one answer belongs.
func TestStreamJoinsMultiRecordAssistantTurnIntoOneMessage(t *testing.T) {
	messages := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"first"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"second"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"third"}]}}`,
	})

	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2 (one user turn and one assistant turn): %#v", len(messages), messages)
	}
	if messages[0].Role != "user" || messages[0].Text != "the question" {
		t.Fatalf("user turn = %#v", messages[0])
	}
	if messages[1].Role != "assistant" {
		t.Fatalf("second turn role = %q, want assistant", messages[1].Role)
	}
	if messages[1].Text != "first\nsecond\nthird" {
		t.Fatalf("assistant turn text = %q, want the three records joined in file order", messages[1].Text)
	}
}

// TestStreamPreservesToolCallOrderWithinOneTurn covers the interleaving that
// makes this more than a text concatenation: tool calls outnumber text blocks
// in the corpus, so a turn is an interleaving of both and the relative order of
// the tool calls has to survive reconstruction.
func TestStreamPreservesToolCallOrderWithinOneTurn(t *testing.T) {
	messages := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"do the work"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"before alpha"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"alpha","input":{"step":1}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"between the calls"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"beta","input":{"step":2}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"after beta"}]}}`,
	})

	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(messages), messages)
	}
	assistant := messages[1]
	if assistant.Text != "before alpha\nbetween the calls\nafter beta" {
		t.Fatalf("assistant text = %q, want the text records joined in file order", assistant.Text)
	}
	if len(assistant.ToolNames) != 2 {
		t.Fatalf("assistant tools = %#v, want alpha then beta", assistant.ToolNames)
	}
	if assistant.ToolNames[0] != "alpha" || assistant.ToolNames[1] != "beta" {
		t.Fatalf("assistant tool order = %#v, want [alpha beta] in file order", assistant.ToolNames)
	}
}

// TestStreamPreservesToolInputOrderAndPayload proves each reconstructed tool
// call still carries its own verbatim input, so joining records does not smear
// one call's arguments onto another.
func TestStreamPreservesToolInputOrderAndPayload(t *testing.T) {
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", t.TempDir())
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	path := writeCursorJSONLTranscript(t, projectsDir, "Users-alice-source-cursor-repo", turnsConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"go"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"read_file","input":{"path":"first.go"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"read_file","input":{"path":"second.go"}}]}}`,
	})

	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("CollectMessages returned error: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(messages), messages)
	}
	assistant := messages[1]
	if !assistant.HasTools || len(assistant.Tools) != 2 {
		t.Fatalf("assistant tools = %#v, want two calls", assistant.Tools)
	}
	wantPaths := []string{"first.go", "second.go"}
	for i, tool := range assistant.Tools {
		var input struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(tool.Input.Raw, &input); err != nil {
			t.Fatalf("tool %d input did not survive as raw JSON: %v", i, err)
		}
		if input.Path != wantPaths[i] {
			t.Fatalf("tool %d input path = %q, want %q", i, input.Path, wantPaths[i])
		}
	}
}

// TestStreamLeavesSingleRecordTurnsIntact guards the common case. Most turns in
// the corpus are a single record, so reconstruction must leave them byte for
// byte as they were while it joins the multi-record turn beside them.
func TestStreamLeavesSingleRecordTurnsIntact(t *testing.T) {
	messages := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"single user turn"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"single assistant turn"},{"type":"tool_use","name":"only_call","input":{}}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"second single user turn"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"joined one"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"joined two"}]}}`,
	})

	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4: %#v", len(messages), messages)
	}
	if messages[0].Text != "single user turn" {
		t.Fatalf("first single-record turn text = %q", messages[0].Text)
	}
	if messages[1].Text != "single assistant turn" {
		t.Fatalf("single-record assistant text = %q", messages[1].Text)
	}
	if len(messages[1].ToolNames) != 1 || messages[1].ToolNames[0] != "only_call" {
		t.Fatalf("single-record assistant tools = %#v, want [only_call]", messages[1].ToolNames)
	}
	if messages[2].Text != "second single user turn" {
		t.Fatalf("second single-record user turn text = %q", messages[2].Text)
	}
	if messages[3].Text != "joined one\njoined two" {
		t.Fatalf("multi-record turn text = %q, want the two records joined", messages[3].Text)
	}
}

// TestStreamSurfacesAFailedTurnToTheReader is the end-to-end version of the
// turn-outcome fix, at the boundary every consumer reads. A turn Cursor recorded
// as failed reached export, search, and context windows as an ordinary finished
// answer, with the failure visible only in a debug log.
func TestStreamSurfacesAFailedTurnToTheReader(t *testing.T) {
	messages := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"partial answer"}]}}`,
		`{"type":"turn_ended","status":"error","error":"context limit"}`,
	})

	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(messages), messages)
	}
	assistant := messages[1]
	if !strings.Contains(assistant.Text, "partial answer") {
		t.Fatalf("assistant text = %q, want the answer preserved", assistant.Text)
	}
	if !strings.Contains(assistant.Text, "[turn ended: error (context limit)]") {
		t.Fatalf("assistant text = %q, want the recorded failure and its reason visible", assistant.Text)
	}

	// The marker has to mean something, so a turn Cursor recorded as finishing
	// normally must not carry it.
	clean := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"the answer"}]}}`,
		`{"type":"turn_ended","status":"success"}`,
	})
	if len(clean) != 2 {
		t.Fatalf("clean messages = %d, want 2: %#v", len(clean), clean)
	}
	if clean[1].Text != "the answer" {
		t.Fatalf("clean assistant text = %q, want only the answer with no marker", clean[1].Text)
	}
}

// TestStreamJoinsMultiRecordUserTurn covers the user side. User runs are mostly
// length one, but the corpus has runs up to 57 records, and the same defect
// applies to them.
func TestStreamJoinsMultiRecordUserTurn(t *testing.T) {
	messages := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"user part one"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"user part two"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"the answer"}]}}`,
	})

	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(messages), messages)
	}
	if messages[0].Role != "user" || messages[0].Text != "user part one\nuser part two" {
		t.Fatalf("user turn = %#v, want the two records joined", messages[0])
	}
	if messages[1].Text != "the answer" {
		t.Fatalf("assistant turn text = %q", messages[1].Text)
	}
}

// TestScanRecordCarriesTitleUncertainty covers the record field. The scan warned
// about a title drawn from incomplete evidence and then dropped the fact, so
// nothing downstream of the scan could tell a guessed title from a read one.
//
// The flag reaches the daemon's own record only. The control server's wire
// record has no field for it, so a CLI or MCP listing still reads it back
// cleared; carrying it to those surfaces needs a wire field.
func TestScanRecordCarriesTitleUncertainty(t *testing.T) {
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", t.TempDir())
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	uncertainPath := writeCursorJSONLTranscript(t, projectsDir, "Users-alice-source-cursor-repo", turnsConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the real firs`,
		`{"role":"user","message":{"content":[{"type":"text","text":"follow-up"}]}}`,
	})
	cleanPath := writeCursorJSONLTranscript(t, projectsDir, "Users-alice-source-cursor-repo", cleanConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the real first"}]}}`,
	})

	parser := New()
	if _, err := parser.Discover(context.Background(), nil); err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}

	uncertain, ok := parser.ScanRecord(uncertainPath, conversation.FileStamp{})
	if !ok {
		t.Fatal("ScanRecord ok = false for the transcript with an unreadable record")
	}
	if !uncertain.TitleUncertain {
		t.Fatalf("record = %#v, want TitleUncertain; a record before the title could not be read", uncertain)
	}

	clean, ok := parser.ScanRecord(cleanPath, conversation.FileStamp{})
	if !ok {
		t.Fatal("ScanRecord ok = false for the clean transcript")
	}
	if clean.TitleUncertain {
		t.Fatalf("record = %#v, want TitleUncertain false; every line read cleanly", clean)
	}
}

// TestStreamSurfacesAnErrorEventsReasonToTheReader is the blocker case. Cursor's
// standalone error event carries its reason with no status, so a reader that
// allowlists failure statuses reads it as a clean finish: an assistant turn
// ending on a rate limit exported byte-identical to one that finished normally,
// with the reason surviving only in a debug log.
func TestStreamSurfacesAnErrorEventsReasonToTheReader(t *testing.T) {
	messages := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"partial answer"}]}}`,
		`{"type":"error","error":"User Provided API Key Rate Limit Exceeded"}`,
	})

	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(messages), messages)
	}
	assistant := messages[1]
	if !strings.Contains(assistant.Text, "partial answer") {
		t.Fatalf("assistant text = %q, want the answer preserved", assistant.Text)
	}
	if !strings.Contains(assistant.Text, "User Provided API Key Rate Limit Exceeded") {
		t.Fatalf("assistant text = %q, want the recorded reason visible", assistant.Text)
	}
	if !strings.Contains(assistant.Text, "[turn ended: error") {
		t.Fatalf("assistant text = %q, want the failure named", assistant.Text)
	}
}

// TestStreamTreatsAnUnknownTurnStatusAsNotClean covers the second trigger of the
// same root. Allowlisting failure statuses means any status Cursor adds later
// reads as a clean finish; success is the allowlist instead.
func TestStreamTreatsAnUnknownTurnStatusAsNotClean(t *testing.T) {
	messages := streamJSONLTurns(t, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"the question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"partial answer"}]}}`,
		`{"type":"turn_ended","status":"throttled","error":"slow down"}`,
	})

	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2: %#v", len(messages), messages)
	}
	if !strings.Contains(messages[1].Text, "[turn ended: throttled (slow down)]") {
		t.Fatalf("assistant text = %q, want a status clyde does not model reported as not clean", messages[1].Text)
	}
}
