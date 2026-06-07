package transcript

import (
	"encoding/json"
	"testing"
)

func TestShapeConversationCompactsToolOnlyTurns(t *testing.T) {
	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools: []ToolCall{
			{Name: "Bash"},
			{Name: "Read"},
		},
	}}, DefaultShapeOptions())
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	if turns[0].Text != "[used: Bash, Read]" {
		t.Fatalf("text=%q", turns[0].Text)
	}
}

func TestShapeConversationOmitsToolOnlyTurns(t *testing.T) {
	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools:    []ToolCall{{Name: "Bash"}},
	}}, ShapeOptions{ToolOnly: ToolOnlyOmit})
	if len(turns) != 0 {
		t.Fatalf("turns=%d want 0", len(turns))
	}
}

func TestShapeConversationConversationOnlyDropsStatusTurns(t *testing.T) {
	turns := ShapeConversation([]Message{
		{Role: "assistant", Text: "No response requested."},
		{Role: "user", Text: "[Request interrupted by user for tool use]"},
		{Role: "user", Text: "Actual user message"},
	}, ShapeOptions{ConversationOnly: true, ToolOnly: ToolOnlyOmit})
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	if turns[0].Text != "Actual user message" {
		t.Fatalf("text=%q want %q", turns[0].Text, "Actual user message")
	}
}

func TestShapeConversationConversationOnlyStripsImagePlaceholderLines(t *testing.T) {
	turns := ShapeConversation([]Message{{
		Role: "user",
		Text: "[Image #1]\n\ncan you align the arrows and double check all the numbers make sense?",
	}}, ShapeOptions{ConversationOnly: true})
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	if turns[0].Text != "can you align the arrows and double check all the numbers make sense?" {
		t.Fatalf("text=%q", turns[0].Text)
	}
}

func TestRenderJSONUsesShapedConversation(t *testing.T) {
	body, err := RenderJSONWithOptions([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools:    []ToolCall{{Name: "Bash"}},
	}}, DefaultShapeOptions())
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	var out []ConversationTurn
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if len(out) != 1 || out[0].Text != "[used: Bash]" {
		t.Fatalf("unexpected json export: %+v", out)
	}
}

func TestCompactionMessagesReturnsOnlyCompactionMessages(t *testing.T) {
	t.Parallel()
	messages := []Message{
		{
			UUID:              "user-1",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "user",
			Visibility:        MessageVisibilityVisible,
			Compaction:        nil,
			Text:              "plain message",
		},
		{
			UUID:              "system-1",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "system",
			Visibility:        MessageVisibilityVisible,
			Compaction: &CompactionMetadata{
				Kind:                    CompactionKindBoundary,
				Trigger:                 CompactionTriggerManual,
				PreTokens:               100,
				PostTokens:              25,
				TokensSaved:             75,
				MessagesSummarized:      0,
				ReplacementHistoryCount: 3,
				HeadUUID:                "head",
				AnchorUUID:              "anchor",
				TailUUID:                "tail",
			},
			Text: "Conversation compacted",
		},
	}

	compactionMessages := CompactionMessages(messages)
	if len(compactionMessages) != 1 {
		t.Fatalf("compaction messages len = %d, want 1", len(compactionMessages))
	}
	if compactionMessages[0].UUID != "system-1" {
		t.Fatalf("compaction message uuid = %q, want system-1", compactionMessages[0].UUID)
	}
	if compactionMessages[0].Compaction == nil || compactionMessages[0].Compaction.Kind != CompactionKindBoundary {
		t.Fatalf("compaction message = %#v", compactionMessages[0].Compaction)
	}
}
