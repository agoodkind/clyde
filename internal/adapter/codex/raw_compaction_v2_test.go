package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

func TestPlanRawResponsesCompactionV2PreservesSetupAndTriggerBytes(t *testing.T) {
	request := RawResponsesRequest{
		Body: []byte(`{"model":"gpt-native","input":[
			{"type":"additional_tools","role":"developer","opaque":{"keep":true}},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"setup"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"oldest"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"oldest answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"older"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"older answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"newer"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
			{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},
			{"type":"custom_tool_call_output","call_id":"call-1","output":"result"},
			{"type":"compaction_trigger","opaque":"trigger"}
		],"metadata":{"keep":true}}`),
		Header: http.Header{CodexTurnMetadataHeader: {`{"session_id":"session-1","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`}},
	}
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}

	plan, ok := PlanRawResponsesCompactionV2(request, settings)
	if !ok {
		t.Fatal("PlanRawResponsesCompactionV2() rejected valid request")
	}
	if plan.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want session-1", plan.SessionID)
	}
	if plan.Transcript == "" || !bytes.Contains([]byte(plan.Transcript), []byte("older")) ||
		bytes.Contains([]byte(plan.Transcript), []byte("newer")) {
		t.Fatalf("Transcript = %q, want only selected completed transcript", plan.Transcript)
	}
	if !bytes.Contains(plan.Request.Body, []byte(`{"type":"additional_tools","role":"developer","opaque":{"keep":true}}`)) ||
		!bytes.Contains(plan.Request.Body, []byte(`{"type":"compaction_trigger","opaque":"trigger"}`)) {
		t.Fatalf("transformed body lost exact setup or trigger bytes: %s", plan.Request.Body)
	}
	if bytes.Contains(plan.Request.Body, []byte(`"older"`)) ||
		!bytes.Contains(plan.Request.Body, []byte(`"newer"`)) ||
		!bytes.Contains(plan.Request.Body, []byte(`"call-1"`)) {
		t.Fatalf("transformed body did not preserve unfinished items: %s", plan.Request.Body)
	}
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(plan.Request.Body, &body); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}
	if len(body.Input) != 9 {
		t.Fatalf("transformed input count = %d, want 9", len(body.Input))
	}
}

func TestPlanRawResponsesCompactionV2PreservesUnfinishedMidTurn(t *testing.T) {
	request := rawResponsesCompactionV2TestRequest(
		`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"oldest"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"oldest answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"finished"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"finished answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"unfinished"}]},
			{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},
			{"type":"compaction_trigger"}
		]`,
		rawResponsesCompactionV2TestMetadata("mid_turn", "memento"),
	)
	request.Header.Set(CodexTurnMetadataHeader, `{"session_id":"session-1","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`)
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}

	plan, ok := PlanRawResponsesCompactionV2(request, settings)
	if !ok {
		t.Fatal("PlanRawResponsesCompactionV2() rejected complete turn prefix")
	}
	if !bytes.Contains(plan.Request.Body, []byte(`"unfinished"`)) ||
		!bytes.Contains(plan.Request.Body, []byte(`"call-1"`)) {
		t.Fatalf("transformed body lost unfinished mid-turn items: %s", plan.Request.Body)
	}
	if bytes.Contains([]byte(plan.Transcript), []byte("unfinished")) ||
		bytes.Contains([]byte(plan.Transcript), []byte("call-1")) {
		t.Fatalf("transcript includes unfinished mid-turn items: %s", plan.Transcript)
	}
}

func TestPlanRawResponsesCompactionV2RendersDeveloperMessages(t *testing.T) {
	request := rawResponsesCompactionV2TestRequest(
		`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"older"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"older answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"newer"}]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"newer developer instruction"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"newer answer"}]},
			{"type":"compaction_trigger"}
		]`,
		rawResponsesCompactionV2TestMetadata("mid_turn", "memento"),
	)
	request.Header.Set(CodexTurnMetadataHeader, `{"session_id":"session-1","request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`)
	plan, ok := PlanRawResponsesCompactionV2(request, RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	})
	if !ok {
		t.Fatal("v2 compaction rejected a developer message in the removable turn")
	}
	if !bytes.Contains([]byte(plan.Transcript), []byte("newer developer instruction")) {
		t.Fatal("v2 recovery transcript omitted the developer message")
	}
}

func TestPlanRawResponsesCompactionV2BoundsRecoveryTranscript(t *testing.T) {
	underRequest := rawResponsesCompactionV2DeveloperRequest(t, "x")
	basePlan, ok := PlanRawResponsesCompactionV2(underRequest, RawResponsesCompactionSettings{Enabled: true, RecentFraction: 0.5})
	if !ok {
		t.Fatal("plan developer boundary fixture")
	}
	underText := strings.Repeat("x", maxRawResponsesCompactionV2RecoveryBytes-len(basePlan.Transcript)+1)
	underRequest = rawResponsesCompactionV2DeveloperRequest(t, underText)
	underPlan, ok := PlanRawResponsesCompactionV2(underRequest, RawResponsesCompactionSettings{Enabled: true, RecentFraction: 0.5})
	if !ok || len(underPlan.Transcript) > maxRawResponsesCompactionV2RecoveryBytes {
		t.Fatalf("under-cap v2 recovery plan ok=%t bytes=%d", ok, len(underPlan.Transcript))
	}

	overText := underText + "xx"
	overRequest := rawResponsesCompactionV2DeveloperRequest(t, overText)
	if _, ok := PlanRawResponsesCompactionV2(overRequest, RawResponsesCompactionSettings{Enabled: true, RecentFraction: 0.5}); ok {
		t.Fatal("over-cap v2 recovery plan unexpectedly succeeded")
	}
}

func rawResponsesCompactionV2DeveloperRequest(t *testing.T, developerText string) RawResponsesRequest {
	t.Helper()
	encodedText, err := json.Marshal(developerText)
	if err != nil {
		t.Fatalf("encode developer fixture: %v", err)
	}
	request := rawResponsesCompactionV2TestRequest(
		fmt.Sprintf(`[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"older"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"older answer"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"newer"}]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":%s}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"newer answer"}]},
			{"type":"compaction_trigger"}
		]`, encodedText),
		rawResponsesCompactionV2TestMetadata("mid_turn", "memento"),
	)
	request.Header.Set(CodexTurnMetadataHeader, `{"session_id":"session-1","request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`)
	return request
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
