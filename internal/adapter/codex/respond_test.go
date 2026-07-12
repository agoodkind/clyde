package codex

import (
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func TestMergeEventsWithNativePatchRepresentationKeepsRouteContract(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: out.md\n+ok\n*** End Patch"
	events := []adapterrender.Event{
		adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
			Index:    0,
			ID:       "call_patch",
			Type:     "function",
			Function: adapteropenai.ToolCallFunction{Name: "ApplyPatch", Arguments: ""},
		}}},
		adapterrender.ToolCallDelta{
			ToolCalls:        []adapteropenai.ToolCall{{Index: 0, Function: adapteropenai.ToolCallFunction{}}},
			NativePatchInput: &adapterrender.NativePatchInput{Input: patch, Final: true},
		},
	}

	tests := []struct {
		name           string
		representation adapterrender.NativePatchRepresentation
		want           string
	}{
		{
			name:           "legacy JSON",
			representation: adapterrender.NativePatchRepresentationJSON,
			want:           `{"input":"*** Begin Patch\n*** Add File: out.md\n+ok\n*** End Patch"}`,
		},
		{
			name:           "native raw",
			representation: adapterrender.NativePatchRepresentationRaw,
			want:           patch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := MergeEventsWithNativePatchRepresentation(
				"chatcmpl_patch",
				"model",
				"",
				events,
				NewRunResult("tool_calls"),
				test.representation,
			)
			calls := response.Choices[0].Message.ToolCalls
			if len(calls) != 1 {
				t.Fatalf("tool calls=%d", len(calls))
			}
			if got := calls[0].Function.Arguments; got != test.want {
				t.Fatalf("arguments=%q want %q", got, test.want)
			}
		})
	}
}
