package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// loadWithToolOutputs streams a transcript body through the parser with tool
// outputs requested, which is the path every caller that wants outputs takes.
func loadWithToolOutputs(t *testing.T, body string) []transcript.Message {
	t.Helper()

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	messages, err := conversation.CollectMessages(New().Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	}))
	if err != nil {
		t.Fatalf("stream fixture: %v", err)
	}
	return messages
}

// assistantCall builds the assistant entry that runs one tool.
func assistantCall(uuid string, callID string, name string) string {
	return `{"uuid":"` + uuid + `","type":"assistant","timestamp":"2026-07-30T12:00:00Z","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"` + callID + `","name":"` + name + `","input":{"command":"true"}}` +
		`]}}` + "\n"
}

// TestAToolResultKeepsItsOutputInEitherShape covers both forms a tool writes its
// result in. A tool returns its result either as a string or as a list of
// content blocks, and a list is what a tool from another server usually returns.
// Both carry text a person read, so both must reach the stored row.
//
// The two results share one transcript entry on purpose. A list-shaped result
// that fails to decode takes the whole entry with it, so a string-shaped result
// sitting beside it loses its output too.
func TestAToolResultKeepsItsOutputInEitherShape(t *testing.T) {
	t.Parallel()

	body := assistantCall("a1", "call-string", "Bash") +
		assistantCall("a2", "call-blocks", "mcp__graphite__run_gt_cmd") +
		`{"uuid":"u1","type":"user","timestamp":"2026-07-30T12:00:01Z","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"call-string","content":"total 8\ndrwxr-xr-x"},` +
		`{"type":"tool_result","tool_use_id":"call-blocks","content":[{"type":"text","text":"06-26-refactor_conversation"}]}` +
		`]}}` + "\n"

	messages := loadWithToolOutputs(t, body)
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	if got := messages[0].Tools[0].Output; !strings.Contains(got, "drwxr-xr-x") {
		t.Fatalf("a string result lost its output: %q", got)
	}
	if got := messages[1].Tools[0].Output; !strings.Contains(got, "06-26-refactor_conversation") {
		t.Fatalf("a list result lost its output: %q", got)
	}
}

// TestAToolResultKeepsEveryBlockItReturned covers a result carrying more than
// one block. Each block is text the person read, so joining them keeps every
// word searchable rather than only the first.
func TestAToolResultKeepsEveryBlockItReturned(t *testing.T) {
	t.Parallel()

	body := assistantCall("a1", "call-1", "mcp__semantic__search_code") +
		`{"uuid":"u1","type":"user","timestamp":"2026-07-30T12:00:01Z","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"call-1","content":[` +
		`{"type":"text","text":"first match"},` +
		`{"type":"text","text":"second match"}` +
		`],"is_error":true}` +
		`]}}` + "\n"

	messages := loadWithToolOutputs(t, body)
	got := messages[0].Tools[0].Output
	for _, want := range []string{"first match", "second match"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing %q: %q", want, got)
		}
	}
	if !messages[0].Tools[0].IsError {
		t.Fatal("the error flag was dropped")
	}
}

// TestAToolResultShapeThisParserCannotReadIsKept covers a result written in
// neither modeled form. Its text is still what the tool returned, so it is kept
// as it arrived rather than dropped for not matching a shape.
func TestAToolResultShapeThisParserCannotReadIsKept(t *testing.T) {
	t.Parallel()

	body := assistantCall("a1", "call-1", "mcp__other__thing") +
		`{"uuid":"u1","type":"user","timestamp":"2026-07-30T12:00:01Z","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"call-1","content":{"rows":42,"note":"nothing matched"}}` +
		`]}}` + "\n"

	messages := loadWithToolOutputs(t, body)
	if got := messages[0].Tools[0].Output; !strings.Contains(got, "nothing matched") {
		t.Fatalf("an unmodeled result shape was dropped: %q", got)
	}
}

// TestAToolResultTravelsWithItsDecodeVerdict pins that the text and the verdict
// cannot be separated. A caller reading the words also receives whether the
// value they came from decoded whole, so an unreadable field cannot resolve to
// an empty string that reads as real content.
func TestAToolResultTravelsWithItsDecodeVerdict(t *testing.T) {
	t.Parallel()

	var complete ToolUseResultContent
	if err := complete.UnmarshalJSON([]byte(`"ran clean"`)); err != nil {
		t.Fatalf("decode string result: %v", err)
	}
	text, decode := complete.SearchableText()
	if text != "ran clean" || decode != FieldDecodeComplete {
		t.Fatalf("SearchableText() = %q, %q; want %q, complete", text, decode, "ran clean")
	}

	var partial ToolUseResultContent
	if err := partial.UnmarshalJSON([]byte(`["not a block"]`)); err != nil {
		t.Fatalf("decode block list: %v", err)
	}
	if _, decode := partial.SearchableText(); decode != FieldDecodePartial {
		t.Fatalf("a value the parser could not read reported %q, want partial", decode)
	}
}
