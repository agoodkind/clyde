package parser

import (
	"encoding/json"
	"testing"
	"time"

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
