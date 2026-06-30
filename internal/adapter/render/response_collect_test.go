package render

import (
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestCollectMessageAssemblesAssistantFields(t *testing.T) {
	events := []Event{
		ReasoningDelta{Text: "Summary", ReasoningKind: "summary", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""},
		ReasoningDelta{Text: "details", ReasoningKind: "text", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""},
		TextDelta{Text: "final "},
		TextDelta{Text: "answer"},
		RefusalDelta{Text: "declined"},
		ToolCallDelta{
			ToolCalls: []adapteropenai.ToolCall{{
				Index: 0,
				ID:    "call_1",
				Type:  "function",
				Function: adapteropenai.ToolCallFunction{
					Name: "ReadFile",
				},
			}},
		},
		ToolCallDelta{
			ToolCalls: []adapteropenai.ToolCall{{
				Index: 0,
				Function: adapteropenai.ToolCallFunction{
					Arguments: `{"path":"README.md"}`,
				},
			}},
		},
	}

	got := CollectMessage(events)
	if got.Text != "final answer" {
		t.Fatalf("text=%q", got.Text)
	}
	if got.Reasoning != "Summary\n\ndetails" {
		t.Fatalf("reasoning=%q", got.Reasoning)
	}
	if got.Refusal != "declined" {
		t.Fatalf("refusal=%q", got.Refusal)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("tool_calls=%d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].Function.Name != "ReadFile" || got.ToolCalls[0].Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("tool_call=%+v", got.ToolCalls[0])
	}
}
