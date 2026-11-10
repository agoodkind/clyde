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

func TestCollectMessageUsesRouteSelectedNativePatchRepresentation(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: out.md\n+ok\n*** End Patch"
	events := []Event{
		ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
			Index:    0,
			ID:       "call_patch",
			Type:     "function",
			Function: adapteropenai.ToolCallFunction{Name: "ApplyPatch", Arguments: ""},
		}}},
		ToolCallDelta{
			ToolCalls:        []adapteropenai.ToolCall{{Index: 0, Function: adapteropenai.ToolCallFunction{}}},
			NativePatchInput: &NativePatchInput{Input: patch, Final: true},
		},
	}

	tests := []struct {
		name           string
		representation NativePatchRepresentation
		want           string
	}{
		{
			name:           "legacy JSON",
			representation: NativePatchRepresentationJSON,
			want:           `{"input":"*** Begin Patch\n*** Add File: out.md\n+ok\n*** End Patch"}`,
		},
		{
			name:           "native raw",
			representation: NativePatchRepresentationRaw,
			want:           patch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collected := CollectMessageWithNativePatchRepresentation(events, test.representation)
			if len(collected.ToolCalls) != 1 {
				t.Fatalf("tool calls=%d", len(collected.ToolCalls))
			}
			if got := collected.ToolCalls[0].Function.Arguments; got != test.want {
				t.Fatalf("arguments=%q want %q", got, test.want)
			}
		})
	}
}
