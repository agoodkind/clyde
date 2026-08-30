package codex

import (
	"net/http"
	"testing"
)

func TestParseRawResponsesCompactionV2CapturedLayout(t *testing.T) {
	request := rawResponsesCompactionV2TestRequest(
		`[
			{"type":"additional_tools","role":"developer"},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"setup"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"older"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},
			{"type":"reasoning","summary":[],"encrypted_content":"cipher"},
			{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},
			{"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"result"}]},
			{"type":"compaction_trigger"}
		]`,
		rawResponsesCompactionV2TestMetadata("mid_turn", "memento"),
	)

	layout, ok := ParseRawResponsesCompactionV2(request)
	if !ok {
		t.Fatal("captured v2 layout rejected")
	}
	if layout.SetupEnd != 2 || layout.TranscriptStart != 2 || layout.TriggerIndex != 7 {
		t.Fatalf("layout = %+v, want SetupEnd=2 TranscriptStart=2 TriggerIndex=7", layout)
	}
}

func TestParseRawResponsesCompactionV2RejectsInvalidLayout(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		metadata string
	}{
		{
			name:  "missing trigger",
			input: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]}]`,
		},
		{
			name:  "nonterminal trigger",
			input: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"compaction_trigger"},{"type":"reasoning","summary":[]}]`,
		},
		{
			name:  "duplicate trigger",
			input: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"compaction_trigger"},{"type":"compaction_trigger"}]`,
		},
		{
			name:  "missing user transcript",
			input: `[{"type":"message","role":"developer","content":[{"type":"input_text","text":"setup"}]},{"type":"compaction_trigger"}]`,
		},
		{
			name:  "unpaired tool",
			input: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},{"type":"compaction_trigger"}]`,
		},
		{
			name:  "duplicate tool call identifier",
			input: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},{"type":"custom_tool_call_output","call_id":"call-1","output":"result"},{"type":"compaction_trigger"}]`,
		},
		{
			name:  "duplicate tool output identifier",
			input: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},{"type":"custom_tool_call_output","call_id":"call-1","output":"result"},{"type":"custom_tool_call_output","call_id":"call-1","output":"result"},{"type":"compaction_trigger"}]`,
		},
		{
			name:     "wrong phase",
			input:    `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"compaction_trigger"}]`,
			metadata: rawResponsesCompactionV2TestMetadata("post_turn", "memento"),
		},
		{
			name:     "wrong strategy",
			input:    `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"compaction_trigger"}]`,
			metadata: rawResponsesCompactionV2TestMetadata("mid_turn", "summary"),
		},
		{
			name:  "unknown transcript item",
			input: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"future_transcript_item"},{"type":"compaction_trigger"}]`,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := testCase.metadata
			if metadata == "" {
				metadata = rawResponsesCompactionV2TestMetadata("mid_turn", "memento")
			}
			request := rawResponsesCompactionV2TestRequest(testCase.input, metadata)
			if layout, ok := ParseRawResponsesCompactionV2(request); ok {
				t.Fatalf("invalid layout accepted: %+v", layout)
			}
		})
	}
}

func rawResponsesCompactionV2TestRequest(input string, metadata string) RawResponsesRequest {
	return RawResponsesRequest{
		Body:   []byte(`{"model":"gpt-native","input":` + input + `}`),
		Header: http.Header{CodexTurnMetadataHeader: {metadata}},
	}
}

func rawResponsesCompactionV2TestMetadata(phase string, strategy string) string {
	return `{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"` +
		phase + `","strategy":"` + strategy + `"}}`
}
