package transcript

import (
	"encoding/json"
	"strings"
	"testing"
)

// mixedTextAndToolsMessage is the shape any provider writes when one turn holds
// both prose and tool calls.
func mixedTextAndToolsMessage() Message {
	return Message{
		Role:     "assistant",
		Text:     "Checking the config now.",
		HasTools: true,
		Tools: []ToolCall{
			{Name: "read_file", Input: ToolInputJSON{Raw: json.RawMessage(`{"path":"main.go"}`)}},
			{Name: "edit_file", Input: ToolInputJSON{Raw: json.RawMessage(`{"path":"main.go"}`)}},
		},
	}
}

// TestShapeConversationRendersToolsAlongsideText is the blocking regression: the
// shaper rendered tool information only when a message had no text, so a turn
// carrying prose and tool calls together lost every tool call before any
// renderer saw it.
func TestShapeConversationRendersToolsAlongsideText(t *testing.T) {
	turns := ShapeConversation([]Message{mixedTextAndToolsMessage()}, DefaultShapeOptions())

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if !strings.Contains(turns[0].Text, "Checking the config now.") {
		t.Fatalf("text = %q, want the prose preserved", turns[0].Text)
	}
	if !strings.Contains(turns[0].Text, "[tool calls in this turn: read_file, edit_file]") {
		t.Fatalf("text = %q, want the turn's tool calls rendered and labeled", turns[0].Text)
	}
	if turns[0].IsToolOnly {
		t.Fatal("IsToolOnly = true, want false; the turn carries prose")
	}
}

// TestShapeConversationDoesNotClaimToolsFollowedTheProse is the honesty
// requirement behind the rendering. Message models a turn as text plus a
// separate tool list, with nothing recording where in the text each call
// happened, so a mixed turn must not read as prose followed by its calls. That
// ordering is unknown, and asserting it is wrong for any turn whose text came
// after a call. A tool-only turn is unambiguous and keeps the bare summary.
func TestShapeConversationDoesNotClaimToolsFollowedTheProse(t *testing.T) {
	mixed := ShapeConversation([]Message{mixedTextAndToolsMessage()}, DefaultShapeOptions())
	if len(mixed) != 1 {
		t.Fatalf("turns = %d, want 1", len(mixed))
	}
	if strings.Contains(mixed[0].Text, "[used: read_file, edit_file]") {
		t.Fatalf("text = %q, want no bare trailing tool summary on a turn that also carries prose", mixed[0].Text)
	}

	toolOnly := ShapeConversation([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools:    []ToolCall{{Name: "read_file"}},
	}}, DefaultShapeOptions())
	if len(toolOnly) != 1 {
		t.Fatalf("tool-only turns = %d, want 1", len(toolOnly))
	}
	if toolOnly[0].Text != "[used: read_file]" {
		t.Fatalf("tool-only text = %q, want the unlabeled summary, whose position is unambiguous", toolOnly[0].Text)
	}
}

// TestShapeConversationRendersFullToolDetailAlongsideText covers the mode
// `--content tools.calls` selects, where the tool input JSON is the point.
func TestShapeConversationRendersFullToolDetailAlongsideText(t *testing.T) {
	turns := ShapeConversation(
		[]Message{mixedTextAndToolsMessage()},
		ShapeOptions{ToolOnly: ToolOnlyFullDetail},
	)

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if !strings.Contains(turns[0].Text, "Checking the config now.") {
		t.Fatalf("text = %q, want the prose preserved", turns[0].Text)
	}
	for _, want := range []string{"[tool calls in this turn]", `[tool: read_file] {"path":"main.go"}`, `[tool: edit_file]`} {
		if !strings.Contains(turns[0].Text, want) {
			t.Fatalf("text = %q, want it to contain %q", turns[0].Text, want)
		}
	}
}

// TestShapeConversationToolModesOnAMixedTurn covers every tool mode against one
// turn that carries prose and tool calls together, which is the shape the mixed
// rendering introduced. The compact case discriminates: before the fix it
// rendered no tools at all. The omit and conversation-only cases pin the two
// modes that must stay free of tool rendering, so the fix cannot leak into them.
func TestShapeConversationToolModesOnAMixedTurn(t *testing.T) {
	compact := ShapeConversation([]Message{mixedTextAndToolsMessage()}, DefaultShapeOptions())
	if len(compact) != 1 || !strings.Contains(compact[0].Text, "read_file") {
		t.Fatalf("compact mode = %#v, want the turn's tool calls rendered", compact)
	}

	omitted := ShapeConversation(
		[]Message{mixedTextAndToolsMessage()},
		ShapeOptions{ToolOnly: ToolOnlyOmit},
	)
	if len(omitted) != 1 {
		t.Fatalf("omit turns = %d, want 1; a turn with prose is not a tool-only turn", len(omitted))
	}
	if omitted[0].Text != "Checking the config now." {
		t.Fatalf("omit text = %q, want only the prose", omitted[0].Text)
	}

	conversationOnly := ShapeConversation(
		[]Message{mixedTextAndToolsMessage()},
		ShapeOptions{ConversationOnly: true},
	)
	if len(conversationOnly) != 1 {
		t.Fatalf("conversation-only turns = %d, want 1", len(conversationOnly))
	}
	if strings.Contains(conversationOnly[0].Text, "tool") {
		t.Fatalf("conversation-only text = %q, want no tool rendering", conversationOnly[0].Text)
	}
}

// TestShapeConversationCapsTheWholeRenderedTurn covers the cap ordering. The
// rune cap was applied to the prose before the tool block was folded in, so a
// capped turn came out longer than its cap by whatever the tools added.
func TestShapeConversationCapsTheWholeRenderedTurn(t *testing.T) {
	const maxRunes = 12

	turns := ShapeConversation(
		[]Message{mixedTextAndToolsMessage()},
		ShapeOptions{ToolOnly: ToolOnlyCompactSummary, MaxTextRunes: maxRunes},
	)

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	// The cap bounds the prose, which is what grows without limit.
	prose, _, found := strings.Cut(turns[0].Text, "\n")
	if !found {
		t.Fatalf("rendered turn = %q, want prose and a tool block", turns[0].Text)
	}
	if runes := []rune(strings.TrimSuffix(prose, "...")); len(runes) > maxRunes {
		t.Fatalf("prose is %d runes, want at most %d: %q", len(runes), maxRunes, turns[0].Text)
	}
	// The tool block survives the cap. Truncating the joined text cut it off a
	// turn whose prose already filled the budget, losing the only record that the
	// turn called anything, which is information the model cannot recover.
	if !strings.Contains(turns[0].Text, "read_file") {
		t.Fatalf("rendered turn = %q, want the tool block kept when the prose is capped", turns[0].Text)
	}
}

// TestRenderMarkdownShowsToolsForMixedTurn checks the whole path a reader sees,
// because the shaper populating ToolNames was never enough: the markdown
// renderer prints only Text and Thinking.
func TestRenderMarkdownShowsToolsForMixedTurn(t *testing.T) {
	turns := ShapeConversation([]Message{mixedTextAndToolsMessage()}, DefaultShapeOptions())
	rendered := RenderMarkdownConversation(turns)

	if !strings.Contains(rendered, "Checking the config now.") {
		t.Fatalf("markdown = %q, want the prose", rendered)
	}
	if !strings.Contains(rendered, "[tool calls in this turn: read_file, edit_file]") {
		t.Fatalf("markdown = %q, want the turn's tool calls to reach the rendered output", rendered)
	}
}

// TestContextWindowShapeOptionsBoundsOneMessage covers the render path a context
// window uses. Grouping made one message a whole turn, and the three render
// paths passed an uncapped shape, so a five-message window around a large turn
// returned most of a megabyte where it used to return five short records.
func TestContextWindowShapeOptionsBoundsOneMessage(t *testing.T) {
	opts := ContextWindowShapeOptions()
	if opts.MaxTextRunes <= 0 {
		t.Fatalf("MaxTextRunes = %d, want a bound; a context window is a preview", opts.MaxTextRunes)
	}

	huge := Message{Role: "assistant", Text: strings.Repeat("x", 300_000)}
	turns := ShapeConversation([]Message{huge}, opts)
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	body := strings.TrimSuffix(turns[0].Text, "...")
	if runes := []rune(body); len(runes) > opts.MaxTextRunes {
		t.Fatalf("rendered message is %d runes, want at most %d", len(runes), opts.MaxTextRunes)
	}

	// An export is the document, so it stays uncapped.
	if DefaultShapeOptions().MaxTextRunes != 0 {
		t.Fatalf("DefaultShapeOptions cap = %d, want 0 so export stays complete", DefaultShapeOptions().MaxTextRunes)
	}
}

// TestFullDetailKeepsToolOutputWhenTheInputIsAbsent covers a call whose input is
// nil. The renderer skipped to the next tool on an absent input, which dropped
// that call's output with it, so a shell call recorded only by what it returned
// rendered as if it returned nothing.
func TestFullDetailKeepsToolOutputWhenTheInputIsAbsent(t *testing.T) {
	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		Text:     "done",
		HasTools: true,
		Tools: []ToolCall{{
			Name:   "Bash",
			Output: "exit 1",
		}},
	}}, ShapeOptions{ToolOnly: ToolOnlyFullDetail})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if !strings.Contains(turns[0].Text, "[tool: Bash]") {
		t.Fatalf("text = %q, want the call named", turns[0].Text)
	}
	if !strings.Contains(turns[0].Text, "exit 1") {
		t.Fatalf("text = %q, want the output kept even with no input", turns[0].Text)
	}
}

// TestFullDetailKeepsToolOutputWhenTheInputIsUnmarshalable covers the other
// branch that skipped, so neither way of failing to render an input can take the
// output with it.
func TestFullDetailKeepsToolOutputWhenTheInputIsUnmarshalable(t *testing.T) {
	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		Text:     "done",
		HasTools: true,
		Tools: []ToolCall{{
			Name:   "Bash",
			Input:  ToolInputJSON{Raw: json.RawMessage("{not json")},
			Output: "exit 1",
		}},
	}}, ShapeOptions{ToolOnly: ToolOnlyFullDetail})

	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if !strings.Contains(turns[0].Text, "exit 1") {
		t.Fatalf("text = %q, want the output kept when the input cannot render", turns[0].Text)
	}
}
