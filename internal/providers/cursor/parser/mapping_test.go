package parser

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/conversation"
	cursorjsonl "goodkind.io/clyde/internal/providers/cursor/jsonl"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
	"goodkind.io/clyde/internal/transcript"
)

func TestMapJSONLMessageMapsRolesTextAndTools(t *testing.T) {
	t.Parallel()

	message, include := mapJSONLMessage(cursorjsonl.TranscriptMessage{
		Role: cursorjsonl.RoleAssistant,
		Parts: []cursorjsonl.ContentPart{
			{Type: cursorjsonl.PartTypeText, Text: "first", ToolName: "", ToolInput: nil},
			{Type: cursorjsonl.PartTypeText, Text: "second", ToolName: "", ToolInput: nil},
			{
				Type:      cursorjsonl.PartTypeToolUse,
				Text:      "",
				ToolName:  "read_file",
				ToolInput: json.RawMessage(`{"path":"README.md"}`),
			},
		},
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if message.Role != "assistant" {
		t.Fatalf("Role = %q, want assistant", message.Role)
	}
	if message.Text != "first\nsecond" {
		t.Fatalf("Text = %q, want joined text", message.Text)
	}
	if message.Visibility != transcript.MessageVisibilityVisible {
		t.Fatalf("Visibility = %q, want visible", message.Visibility)
	}
	if !message.HasTools {
		t.Fatal("HasTools = false, want true")
	}
	if len(message.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1", len(message.Tools))
	}
	tool := message.Tools[0]
	if tool.Name != "read_file" {
		t.Fatalf("Tool.Name = %q, want read_file", tool.Name)
	}
	if string(tool.Input.Raw) != `{"path":"README.md"}` {
		t.Fatalf("Tool.Input.Raw = %s", tool.Input.Raw)
	}
}

func TestMapComposerBubbleMapsRolesThinkingAndToolOutputGate(t *testing.T) {
	t.Parallel()

	assistant := cursorstore.Bubble{
		BubbleID:      "bubble-a",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          "done",
		Thinking:      cursorstore.BubbleThinking{Text: "thought"},
		ToolCall: &cursorstore.BubbleToolCall{
			Name:    "run_command",
			RawArgs: `{"cmd":"date"}`,
			Result:  "output",
			Status:  "failed",
		},
	}

	withoutOutput, include := mapComposerBubble(assistant, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if withoutOutput.Role != "assistant" {
		t.Fatalf("Role = %q, want assistant", withoutOutput.Role)
	}
	if withoutOutput.Thinking != "thought" {
		t.Fatalf("Thinking = %q, want thought", withoutOutput.Thinking)
	}
	if len(withoutOutput.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1", len(withoutOutput.Tools))
	}
	if withoutOutput.Tools[0].Output != "" {
		t.Fatalf("tool output = %q, want empty", withoutOutput.Tools[0].Output)
	}
	if !withoutOutput.Tools[0].IsError {
		t.Fatal("IsError = false, want true")
	}

	withOutput, include := mapComposerBubble(assistant, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	})
	if !include {
		t.Fatal("include with output = false, want true")
	}
	if withOutput.Tools[0].Output != "output" {
		t.Fatalf("tool output = %q, want output", withOutput.Tools[0].Output)
	}

	user, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-user",
		Type:          cursorstore.BubbleTypeUser,
		SchemaVersion: 3,
		Text:          "hello",
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("user include = false, want true")
	}
	if user.Role != "user" {
		t.Fatalf("user Role = %q, want user", user.Role)
	}
}

// TestMapComposerBubbleCarriesTheStoredWriteTime pins the timestamp a reader
// sees to the one that ordered the chat. The same value places the bubble during
// assembly, so a message that carried it in one place and not the other would be
// describing two different conversations.
func TestMapComposerBubbleCarriesTheStoredWriteTime(t *testing.T) {
	t.Parallel()

	dated, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:       "bubble-dated",
		Type:           cursorstore.BubbleTypeUser,
		SchemaVersion:  3,
		Text:           "hello",
		Thinking:       cursorstore.BubbleThinking{Text: ""},
		ToolCall:       nil,
		CreatedAt:      "2026-05-06T05:00:30.500Z",
		ServerBubbleID: "",
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	want := time.Date(2026, time.May, 6, 5, 0, 30, 500_000_000, time.UTC)
	if !dated.Timestamp.Equal(want) {
		t.Fatalf("Timestamp = %s, want %s", dated.Timestamp, want)
	}

	undated, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:       "bubble-undated",
		Type:           cursorstore.BubbleTypeUser,
		SchemaVersion:  3,
		Text:           "hello",
		Thinking:       cursorstore.BubbleThinking{Text: ""},
		ToolCall:       nil,
		CreatedAt:      "",
		ServerBubbleID: "",
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("undated include = false, want true")
	}
	if !undated.Timestamp.IsZero() {
		t.Fatalf("undated Timestamp = %s, want the zero time", undated.Timestamp)
	}
}

func TestMapComposerBubbleWrapsNonJSONArgs(t *testing.T) {
	t.Parallel()

	message, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-tool",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          "",
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall: &cursorstore.BubbleToolCall{
			Name:    "tool",
			RawArgs: "plain text",
			Result:  "",
			Status:  "success",
		},
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if string(message.Tools[0].Input.Raw) != `"plain text"` {
		t.Fatalf("Input.Raw = %s, want JSON string", message.Tools[0].Input.Raw)
	}
}

func TestMapLegacyBubbleMapsStringRoles(t *testing.T) {
	t.Parallel()

	user, include := mapLegacyBubble(cursorstore.LegacyChatBubble{
		Type: cursorstore.LegacyChatRoleUser,
		Text: "hello",
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("user include = false, want true")
	}
	if user.Role != "user" {
		t.Fatalf("user Role = %q, want user", user.Role)
	}

	assistant, include := mapLegacyBubble(cursorstore.LegacyChatBubble{
		Type: cursorstore.LegacyChatRoleAssistant,
		Text: "hi",
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("assistant include = false, want true")
	}
	if assistant.Role != "assistant" {
		t.Fatalf("assistant Role = %q, want assistant", assistant.Role)
	}
}

func TestMappingSkipsEmptyMessages(t *testing.T) {
	t.Parallel()

	_, includeJSONL := mapJSONLMessage(cursorjsonl.TranscriptMessage{
		Role:  cursorjsonl.RoleUser,
		Parts: nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if includeJSONL {
		t.Fatal("empty JSONL include = true, want false")
	}

	_, includeComposer := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "empty",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          "",
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if includeComposer {
		t.Fatal("empty composer include = true, want false")
	}

	_, includeLegacy := mapLegacyBubble(cursorstore.LegacyChatBubble{
		Type: cursorstore.LegacyChatRoleAssistant,
		Text: "",
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if includeLegacy {
		t.Fatal("empty legacy include = true, want false")
	}
}

// synthenticNoPaddingBoundaryEnvelope and its exact stripped-and-rejoined
// result are shared by the three no-padding boundary tests below. The input
// carries no whitespace at all between the prose and the marker on either
// side, so a pass whose separator logic is missing or wrong (concatenating
// the surviving text spans directly) fuses "Alpha" and "Bravo" into one
// word instead of producing this exact, precisely computed string.
const synthenticNoPaddingBoundaryEnvelopeBody = "note"

func synthenticNoPaddingBoundaryText() string {
	return "Alpha" + adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, synthenticNoPaddingBoundaryEnvelopeBody) + "Bravo"
}

const synthenticNoPaddingBoundaryWant = "Alpha\n\nBravo"

func TestMapComposerBubbleStripsSyntheticEnvelopeExactOutputNoPadding(t *testing.T) {
	t.Parallel()

	message, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-notice",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          synthenticNoPaddingBoundaryText(),
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if message.Text != synthenticNoPaddingBoundaryWant {
		t.Fatalf("Text = %q, want exactly %q", message.Text, synthenticNoPaddingBoundaryWant)
	}
}

func TestMapComposerBubbleNoMarkerByteIdentical(t *testing.T) {
	t.Parallel()

	const plain = "Plain assistant reply with no envelopes at all."
	message, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-plain",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          plain,
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if message.Text != plain {
		t.Fatalf("Text = %q, want byte-identical %q", message.Text, plain)
	}
}

// TestMapComposerBubbleUserRoleNeverStripped is the regression test for the
// failure the workstream exists to prevent: Clyde injects synthetic
// envelopes into the assistant stream only, so a marker match on a user
// bubble is never Clyde's own content. It is the user's own text (e.g. a
// pasted transcript or source file that happens to quote the marker
// syntax). The worst case is a user message that is ENTIRELY a complete
// marker pair: stripping it would empty the message, and the existing
// empty-message gate would then drop the user's message from the
// conversation outright.
//
// The user-role assertion is paired with the same text run as an
// assistant bubble, which must come back stripped. Without the second
// half, "user text is unchanged" would also hold for a filter that does
// nothing at all; asserting both directions on the same input proves the
// filter discriminates by role rather than being a no-op.
func TestMapComposerBubbleUserRoleNeverStripped(t *testing.T) {
	t.Parallel()

	pasted := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "not real reasoning, just a paste")

	user, includeUser := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-user-pasted",
		Type:          cursorstore.BubbleTypeUser,
		SchemaVersion: 3,
		Text:          pasted,
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !includeUser {
		t.Fatal("user include = false, want true: a user message must never disappear because it quotes the marker syntax")
	}
	if user.Text != pasted {
		t.Fatalf("user Text = %q, want byte-identical %q (user text must never be stripped)", user.Text, pasted)
	}

	assistant, includeAssistant := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-assistant-same-text",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          pasted,
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if includeAssistant {
		t.Fatalf("assistant include = true, want false: the same text as an assistant bubble is only an envelope and must collapse to empty")
	}
	_ = assistant
}

// TestMapComposerBubbleUnmatchedMarkerNeverStripped covers CLYDE-601's
// second-round regression: treating an unmatched marker as proof of
// Clyde's own content is a determination the evidence does not support.
// An unmatched marker is equally consistent with assistant prose that
// mentions or quotes the marker syntax, which is ordinary traffic in a
// repository that builds this exact feature. Two shapes are covered: a
// literal mention of the close marker with no open anywhere (this
// repository's actual traffic, not a hypothetical), and a dangling open
// marker with no close (what an aborted turn would look like). Neither
// may empty the message or alter it.
func TestMapComposerBubbleUnmatchedMarkerNeverStripped(t *testing.T) {
	t.Parallel()

	t.Run("close_marker_mentioned_in_prose", func(t *testing.T) {
		t.Parallel()
		const text = "The closing marker is <!--/clyde-notice-->"
		message, include := mapComposerBubble(cursorstore.Bubble{
			BubbleID:      "bubble-mentions-close",
			Type:          cursorstore.BubbleTypeAssistant,
			SchemaVersion: 3,
			Text:          text,
			Thinking:      cursorstore.BubbleThinking{Text: ""},
			ToolCall:      nil,
		}, conversation.LoadOptions{
			IncludeSystemPrompts:  false,
			IncludeSystemMessages: false,
			IncludeToolOutputs:    false,
		})
		if !include {
			t.Fatal("include = false, want true: a message merely mentioning the marker must not disappear")
		}
		if message.Text != text {
			t.Fatalf("Text = %q, want byte-identical %q", message.Text, text)
		}
	})

	t.Run("dangling_open_marker_no_close", func(t *testing.T) {
		t.Parallel()
		abortedTail := adapterrender.FormatSyntheticContentDeltaWithRef(
			adapterrender.SyntheticReasoning, true, true, "", adapterrender.OriginUnknown,
			"partial reasoning cut short by an abort",
		)
		text := "Before.\n\n" + abortedTail // no close marker anywhere

		message, include := mapComposerBubble(cursorstore.Bubble{
			BubbleID:      "bubble-dangling-open",
			Type:          cursorstore.BubbleTypeAssistant,
			SchemaVersion: 3,
			Text:          text,
			Thinking:      cursorstore.BubbleThinking{Text: ""},
			ToolCall:      nil,
		}, conversation.LoadOptions{
			IncludeSystemPrompts:  false,
			IncludeSystemMessages: false,
			IncludeToolOutputs:    false,
		})
		if !include {
			t.Fatal("include = false, want true")
		}
		if message.Text != text {
			t.Fatalf("Text = %q, want byte-identical %q (an unmatched open marker must not be treated as Clyde's content)", message.Text, text)
		}
	})
}

// TestMapComposerBubbleUnknownTypeNeverStripped covers CLYDE-601's
// second-round finding #2: the strip gate must require bubble.Type to say
// assistant explicitly, not merely "not user". A bubble whose type is
// neither BubbleTypeUser nor BubbleTypeAssistant (a partial write, or a
// type value this parser does not yet model) must not fall into the
// destructive branch by default.
func TestMapComposerBubbleUnknownTypeNeverStripped(t *testing.T) {
	t.Parallel()

	const unknownBubbleType = 0 // neither BubbleTypeUser (1) nor BubbleTypeAssistant (2)
	pasted := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "user-authored text that happens to quote the marker")

	message, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-unknown-type",
		Type:          unknownBubbleType,
		SchemaVersion: 3,
		Text:          pasted,
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true: an unrecognized bubble type must not default to the destructive strip branch")
	}
	if message.Text != pasted {
		t.Fatalf("Text = %q, want byte-identical %q", message.Text, pasted)
	}
}

// TestMapComposerBubbleIndentedCodeAfterEnvelopeSurvives covers CLYDE-601's
// second-round finding #3: leading whitespace immediately after a stripped
// envelope is content, not decoration, whenever it belongs to what
// follows (e.g. an indented code block line). It must survive the strip
// intact.
func TestMapComposerBubbleIndentedCodeAfterEnvelopeSurvives(t *testing.T) {
	t.Parallel()

	envelope := adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, "heads up")
	text := envelope + "    indented code line"

	message, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-indented-code",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          text,
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if message.Text != "    indented code line" {
		t.Fatalf("Text = %q, want %q with leading indentation intact", message.Text, "    indented code line")
	}
}

// TestMapComposerBubbleFullyStrippedEnvelopeExcludesMessage covers the
// interaction between the strip and the pre-existing empty-message gate:
// an assistant bubble whose entire content is a synthetic envelope, with no
// other prose, no live Thinking text, and no tool call, must collapse to
// empty and be excluded exactly like any other empty message.
func TestMapComposerBubbleFullyStrippedEnvelopeExcludesMessage(t *testing.T) {
	t.Parallel()

	envelope := adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, "just a notice")
	_, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-only-envelope",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          envelope,
		Thinking:      cursorstore.BubbleThinking{Text: ""},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if include {
		t.Fatal("include = true, want false: a bubble that is only a synthetic envelope has nothing left once stripped")
	}
}

// TestMapComposerBubbleDualSurfaceReasoningKeepsThinkingFieldSeparate locks
// in the current, confirmed-by-code behavior for CLYDE-601 finding #4:
// Cursor's Bubble.Thinking field is fed by Clyde's dual-emitted
// reasoning_content stream (render.ReasoningRenderModeDualSurface, the
// Cursor default: see internal/adapter/render/reasoning_render_mode.go and
// internal/adapter/render/synthetic_reasoning.go, which emit the same
// decorated body on Content and ReasoningContent on the same chunk). The
// same reasoning body can therefore legitimately reach this bubble twice:
// once marker-wrapped in Text, once raw in Thinking. This test asserts the
// wrapped copy is stripped from Text while the pre-existing Thinking field
// is left untouched, because Thinking is Cursor's own distinct field (not
// something this filter introduced) and is already excluded from the
// default semantic indexing content policy
// (internal/daemon/conversation_semantic_content_policy.go), which is the
// read path CLYDE-601 targets.
func TestMapComposerBubbleDualSurfaceReasoningKeepsThinkingFieldSeparate(t *testing.T) {
	t.Parallel()

	const duplicatedReasoning = "duplicated reasoning body"
	envelope := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, duplicatedReasoning)
	message, include := mapComposerBubble(cursorstore.Bubble{
		BubbleID:      "bubble-dual-surface",
		Type:          cursorstore.BubbleTypeAssistant,
		SchemaVersion: 3,
		Text:          "Answer." + envelope,
		Thinking:      cursorstore.BubbleThinking{Text: duplicatedReasoning},
		ToolCall:      nil,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if !strings.Contains(message.Text, "Answer.") {
		t.Fatalf("Text = %q, want the visible answer preserved", message.Text)
	}
	if strings.Contains(message.Text, duplicatedReasoning) {
		t.Fatalf("Text = %q, want the marker-wrapped copy stripped", message.Text)
	}
	if message.Thinking != duplicatedReasoning {
		t.Fatalf("Thinking = %q, want the pre-existing dual-surface field left untouched: %q", message.Thinking, duplicatedReasoning)
	}
}

func TestMapLegacyBubbleStripsSyntheticEnvelopeExactOutputNoPadding(t *testing.T) {
	t.Parallel()

	message, include := mapLegacyBubble(cursorstore.LegacyChatBubble{
		Type: cursorstore.LegacyChatRoleAssistant,
		Text: synthenticNoPaddingBoundaryText(),
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if message.Text != synthenticNoPaddingBoundaryWant {
		t.Fatalf("Text = %q, want exactly %q", message.Text, synthenticNoPaddingBoundaryWant)
	}
}

// TestMapLegacyBubbleUserRoleNeverStripped is the legacy-store twin of
// TestMapComposerBubbleUserRoleNeverStripped: see that test for the failure
// this prevents and why the assistant counterpart is asserted alongside
// the user case (a no-op filter would otherwise satisfy the user
// assertion alone).
func TestMapLegacyBubbleUserRoleNeverStripped(t *testing.T) {
	t.Parallel()

	pasted := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "not real reasoning, just a paste")

	user, includeUser := mapLegacyBubble(cursorstore.LegacyChatBubble{
		Type: cursorstore.LegacyChatRoleUser,
		Text: pasted,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !includeUser {
		t.Fatal("user include = false, want true: a user message must never disappear because it quotes the marker syntax")
	}
	if user.Text != pasted {
		t.Fatalf("user Text = %q, want byte-identical %q (user text must never be stripped)", user.Text, pasted)
	}

	_, includeAssistant := mapLegacyBubble(cursorstore.LegacyChatBubble{
		Type: cursorstore.LegacyChatRoleAssistant,
		Text: pasted,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if includeAssistant {
		t.Fatalf("assistant include = true, want false: the same text as an assistant bubble is only an envelope and must collapse to empty")
	}
}

// TestMapLegacyBubbleFullyStrippedEnvelopeExcludesMessage is the
// legacy-store twin of TestMapComposerBubbleFullyStrippedEnvelopeExcludesMessage.
// The legacy bubble shape has no Thinking field at all, so an
// envelope-only assistant bubble has strictly nothing left once stripped.
func TestMapLegacyBubbleFullyStrippedEnvelopeExcludesMessage(t *testing.T) {
	t.Parallel()

	envelope := adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, "just a notice")
	_, include := mapLegacyBubble(cursorstore.LegacyChatBubble{
		Type: cursorstore.LegacyChatRoleAssistant,
		Text: envelope,
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if include {
		t.Fatal("include = true, want false: a bubble that is only a synthetic envelope has nothing left once stripped")
	}
}

func TestMapJSONLMessageStripsSyntheticEnvelopeExactOutputNoPadding(t *testing.T) {
	t.Parallel()

	message, include := mapJSONLMessage(cursorjsonl.TranscriptMessage{
		Role: cursorjsonl.RoleAssistant,
		Parts: []cursorjsonl.ContentPart{
			{Type: cursorjsonl.PartTypeText, Text: synthenticNoPaddingBoundaryText(), ToolName: "", ToolInput: nil},
		},
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !include {
		t.Fatal("include = false, want true")
	}
	if message.Text != synthenticNoPaddingBoundaryWant {
		t.Fatalf("Text = %q, want exactly %q", message.Text, synthenticNoPaddingBoundaryWant)
	}
}

// TestMapJSONLMessageUserRoleNeverStripped is the JSONL-store twin of
// TestMapComposerBubbleUserRoleNeverStripped: see that test for the failure
// this prevents and why the assistant counterpart is asserted alongside
// the user case (a no-op filter would otherwise satisfy the user
// assertion alone).
func TestMapJSONLMessageUserRoleNeverStripped(t *testing.T) {
	t.Parallel()

	pasted := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "not real reasoning, just a paste")

	user, includeUser := mapJSONLMessage(cursorjsonl.TranscriptMessage{
		Role: cursorjsonl.RoleUser,
		Parts: []cursorjsonl.ContentPart{
			{Type: cursorjsonl.PartTypeText, Text: pasted, ToolName: "", ToolInput: nil},
		},
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if !includeUser {
		t.Fatal("user include = false, want true: a user message must never disappear because it quotes the marker syntax")
	}
	if user.Text != pasted {
		t.Fatalf("user Text = %q, want byte-identical %q (user text must never be stripped)", user.Text, pasted)
	}

	_, includeAssistant := mapJSONLMessage(cursorjsonl.TranscriptMessage{
		Role: cursorjsonl.RoleAssistant,
		Parts: []cursorjsonl.ContentPart{
			{Type: cursorjsonl.PartTypeText, Text: pasted, ToolName: "", ToolInput: nil},
		},
	}, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if includeAssistant {
		t.Fatalf("assistant include = true, want false: the same text as an assistant role is only an envelope and must collapse to empty")
	}
}
