package parser

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/transcript"
)

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

	messages := []transcript.Message{{
		Role: "assistant",
		Tools: []transcript.ToolCall{
			{ID: "call-string", Name: "Bash"},
			{ID: "call-blocks", Name: "mcp__graphite__run_gt_cmd"},
		},
	}}

	body := `{"type":"user","message":{"content":[` +
		`{"type":"tool_result","tool_use_id":"call-string","content":"total 8\ndrwxr-xr-x"},` +
		`{"type":"tool_result","tool_use_id":"call-blocks","content":[{"type":"text","text":"06-26-refactor_conversation"}]}` +
		`]}}` + "\n"

	attachToolOutputs([]byte(body), messages)

	tools := messages[0].Tools
	if got := tools[0].Output; !strings.Contains(got, "drwxr-xr-x") {
		t.Fatalf("a string result lost its output: %q", got)
	}
	if got := tools[1].Output; !strings.Contains(got, "06-26-refactor_conversation") {
		t.Fatalf("a list result lost its output: %q", got)
	}
}

// TestAToolResultKeepsEveryBlockItReturned covers a result carrying more than
// one block. Each block is text the person read, so joining them keeps every
// word searchable rather than only the first.
func TestAToolResultKeepsEveryBlockItReturned(t *testing.T) {
	t.Parallel()

	messages := []transcript.Message{{
		Role:  "assistant",
		Tools: []transcript.ToolCall{{ID: "call-1", Name: "mcp__semantic__search_code"}},
	}}

	body := `{"type":"user","message":{"content":[` +
		`{"type":"tool_result","tool_use_id":"call-1","content":[` +
		`{"type":"text","text":"first match"},` +
		`{"type":"text","text":"second match"}` +
		`],"is_error":true}` +
		`]}}` + "\n"

	attachToolOutputs([]byte(body), messages)

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

	messages := []transcript.Message{{
		Role:  "assistant",
		Tools: []transcript.ToolCall{{ID: "call-1", Name: "mcp__other__thing"}},
	}}

	body := `{"type":"user","message":{"content":[` +
		`{"type":"tool_result","tool_use_id":"call-1","content":{"rows":42,"note":"nothing matched"}}` +
		`]}}` + "\n"

	attachToolOutputs([]byte(body), messages)

	if got := messages[0].Tools[0].Output; !strings.Contains(got, "nothing matched") {
		t.Fatalf("an unmodeled result shape was dropped: %q", got)
	}
}
