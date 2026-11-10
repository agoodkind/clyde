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
			{Name: "Bash", Display: "", DisplayLang: ""},
			{Name: "Read", Display: "", DisplayLang: ""},
		},
	}}, DefaultShapeOptions())
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	if turns[0].Text != "[used: Bash, Read]" {
		t.Fatalf("text=%q", turns[0].Text)
	}
}

func TestShapeConversationUsesToolInputSummary(t *testing.T) {
	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools: []ToolCall{
			{
				Name:        "Bash",
				Input:       ToolInputJSON{Raw: json.RawMessage(`{"command":"echo \"===","description":"Diagnose streaming probe timeout: SSE bytes, crash, memory"}`)},
				Display:     "",
				DisplayLang: "",
			},
			{
				Name:        "Read",
				Input:       ToolInputJSON{Raw: json.RawMessage(`{"file_path":"/tmp/probe.log"}`)},
				Display:     "",
				DisplayLang: "",
			},
		},
	}}, ShapeOptions{ToolOnly: ToolOnlyInputSummary})
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	want := "[tool: Bash] Diagnose streaming probe timeout: SSE bytes, crash, memory\n[tool: Read]"
	if turns[0].Text != want {
		t.Fatalf("text=%q want %q", turns[0].Text, want)
	}
}

func TestShapeConversationOmitsToolOnlyTurns(t *testing.T) {
	turns := ShapeConversation([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools:    []ToolCall{{Name: "Bash", Display: "", DisplayLang: ""}},
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

func TestShapeConversationConversationOnlyKeepsAttachmentOnlyTurns(t *testing.T) {
	attachment := Attachment{Kind: "image", DisplayName: "diagram.png"}
	turns := ShapeConversation([]Message{{
		Role:        "user",
		Text:        "[Image #1]",
		HasTools:    true,
		Tools:       []ToolCall{{Name: "Read"}},
		Attachments: []Attachment{attachment},
	}}, ShapeOptions{ConversationOnly: true, ToolOnly: ToolOnlyOmit})
	if len(turns) != 1 {
		t.Fatalf("turns=%d want 1", len(turns))
	}
	if len(turns[0].Attachments) != 1 || turns[0].Attachments[0] != attachment {
		t.Fatalf("attachments=%+v want %+v", turns[0].Attachments, attachment)
	}
}

func TestRenderJSONUsesShapedConversation(t *testing.T) {
	body, err := RenderJSONWithOptions([]Message{{
		Role:     "assistant",
		HasTools: true,
		Tools:    []ToolCall{{Name: "Bash", Display: "", DisplayLang: ""}},
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
	contextItem := CompactedContextItem{
		Kind: CompactedContextItemKindMessage,
		Message: &CompactedMessageItem{
			Role:         "user",
			Phase:        "",
			Content:      []CompactedMessageContentItem{{Type: "input_text", Text: "summary", Raw: json.RawMessage(`"summary"`)}},
			ContentRaw:   json.RawMessage(`[{"type":"input_text","text":"summary"}]`),
			MessageClass: CompactedMessageClassSummary,
			Raw:          json.RawMessage(`{"type":"message","role":"user"}`),
		},
	}
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
				ContextItems:            []CompactedContextItem{contextItem},
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
	if len(compactionMessages[0].Compaction.ContextItems) != 1 {
		t.Fatalf("context items len = %d, want 1", len(compactionMessages[0].Compaction.ContextItems))
	}
	if compactionMessages[0].Compaction.ContextItems[0].Message == nil {
		t.Fatalf("context item message was nil")
	}
	if compactionMessages[0].Compaction.ContextItems[0].Message.Content[0].Text != "summary" {
		t.Fatalf("copied summary text = %q", compactionMessages[0].Compaction.ContextItems[0].Message.Content[0].Text)
	}
}
