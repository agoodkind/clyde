package codex

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
)

func TestPlanRawResponsesCompactionUsesTranscriptIndexesAndPreservesPrompt(t *testing.T) {
	body := []byte(`{"model":"gpt-native","input":[ {"type":"message","role":"user","content":[{"type":"input_text","text":"old user"}]}, {"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]}, {"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]}, {"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent assistant"}]}, { "type" : "message", "role" : "user", "content" : [{"type":"input_text","text":"summarize\\nexact"}] } ],"tools":[{"type":"custom","name":"opaque"}],"opaque":{"keep":true}}`)
	originalInput := rawInputItemsForTest(t, body)
	transformed, transformer := PrepareRawResponsesCompaction(
		rawCompactionRequest(t, body),
		RawResponsesCompactionSettings{
			Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
			ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
		},
	)
	if transformer == nil {
		t.Fatal("expected a compaction transformer")
	}
	gotInput := rawInputItemsForTest(t, transformed.Body)
	if len(gotInput) != 3 {
		t.Fatalf("input items = %d, want two older items plus prompt", len(gotInput))
	}
	if !bytes.Equal(gotInput[0], originalInput[0]) || !bytes.Equal(gotInput[1], originalInput[1]) {
		t.Fatalf("older input changed:\n%s", transformed.Body)
	}
	if !bytes.Equal(gotInput[2], originalInput[4]) {
		t.Fatalf("prompt bytes changed:\n got: %s\nwant: %s", gotInput[2], originalInput[4])
	}
	if !bytes.Contains(transformed.Body, []byte(`"tools":[{"type":"custom","name":"opaque"}]`)) ||
		!bytes.Contains(transformed.Body, []byte(`"opaque":{"keep":true}`)) {
		t.Fatalf("unrelated request fields changed: %s", transformed.Body)
	}
	response := transformer.TransformResponse(rawJSONResponse(http.StatusOK, "summary"))
	responseBody := readResponseBody(t, response)
	if !bytes.Contains(responseBody, []byte("recent user")) || !bytes.Contains(responseBody, []byte("recent assistant")) {
		t.Fatalf("response missing removed transcript: %s", responseBody)
	}
	if bytes.Contains(responseBody, []byte("old user")) {
		t.Fatalf("response included retained transcript: %s", responseBody)
	}
}

func TestPlanRawResponsesCompactionHonorsFractionAndByteCapBoundaries(t *testing.T) {
	items := rawInputItemsForTest(t, []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"m0"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"m1"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"m2"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"m3"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"m4"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"m5"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}
	]}`))
	fractionPlan, ok := planRawResponsesCompaction(items, 0, 0.5)
	if !ok || fractionPlan.removedStart != 4 || fractionPlan.promptIndex != 6 {
		t.Fatalf("fraction plan = %+v ok=%t, want complete turn [4:6) removed", fractionPlan, ok)
	}
	lastOnly, ok := planRawResponsesCompaction(items, 0, 0.01)
	if ok {
		t.Fatalf("fraction below one item unexpectedly split: %+v", lastOnly)
	}
	lastTurn, ok := renderRawResponsesCompactionNormalizedItems(
		codexstore.NormalizeResponseInputItems(items[4:6]),
	)
	if !ok {
		t.Fatal("last turn did not render")
	}
	capPlan, ok := planRawResponsesCompaction(items, len(lastTurn), 0.5)
	if !ok || capPlan.removedStart != 4 {
		t.Fatalf("cap plan = %+v ok=%t, want complete turn [4:6) removed", capPlan, ok)
	}
	underCap, ok := planRawResponsesCompaction(items, len(lastTurn)-1, 0.5)
	if ok {
		t.Fatalf("under-cap plan unexpectedly split: %+v", underCap)
	}
}

func TestRawResponsesCompactionExpandsEverySupportedToolPair(t *testing.T) {
	tests := []struct {
		name       string
		call       string
		output     string
		wantRender string
	}{
		{
			name:       "function",
			call:       `{"type":"function_call","name":"lookup","arguments":"{\"query\":\"x\"}","call_id":"call-1"}`,
			output:     `{"type":"function_call_output","call_id":"call-1","output":"function result"}`,
			wantRender: "function result",
		},
		{
			name:       "custom",
			call:       `{"type":"custom_tool_call","name":"apply_patch","input":"patch body","call_id":"call-1"}`,
			output:     `{"type":"custom_tool_call_output","name":"apply_patch","call_id":"call-1","output":"custom result"}`,
			wantRender: "custom result",
		},
		{
			name:       "local shell",
			call:       `{"type":"local_shell_call","call_id":"call-1","action":{"type":"exec","command":"pwd"}}`,
			output:     `{"type":"function_call_output","call_id":"call-1","output":"shell result"}`,
			wantRender: "shell result",
		},
		{
			name:       "tool search",
			call:       `{"type":"tool_search_call","call_id":"call-1","arguments":{"query":"calendar"}}`,
			output:     `{"type":"tool_search_output","call_id":"call-1","tools":[{"name":"calendar.search"}]}`,
			wantRender: "calendar.search",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-native","input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"recent"}]},` +
				testCase.call + `,` + testCase.output + `,` +
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent assistant"}]},` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}` +
				`]}`)
			transformed, transformer := PrepareRawResponsesCompaction(
				rawCompactionRequest(t, body),
				RawResponsesCompactionSettings{
					Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
					ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
				},
			)
			if transformer == nil {
				t.Fatal("expected a compaction transformer")
			}
			transformedText := string(transformed.Body)
			if strings.Contains(transformedText, `"call_id":"call-1"`) {
				t.Fatalf("paired items were not both removed: %s", transformedText)
			}
			responseBody := readResponseBody(t, transformer.TransformResponse(rawJSONResponse(http.StatusOK, "summary")))
			if !bytes.Contains(responseBody, []byte(testCase.wantRender)) {
				t.Fatalf("rendered response missing %q: %s", testCase.wantRender, responseBody)
			}
		})
	}
}

func TestRawResponsesCompactionRejectsIncompleteToolPairs(t *testing.T) {
	tests := []struct {
		name  string
		items string
	}{
		{name: "call without id", items: `{"type":"function_call","name":"lookup","arguments":"{}"}`},
		{name: "output without id", items: `{"type":"function_call_output","output":"result"}`},
		{name: "output without payload", items: `{"type":"function_call","name":"lookup","arguments":"{}","call_id":"call-1"},{"type":"function_call_output","call_id":"call-1"}`},
		{name: "output null payload", items: `{"type":"function_call","name":"lookup","arguments":"{}","call_id":"call-1"},{"type":"function_call_output","call_id":"call-1","output":null}`},
		{name: "call without output", items: `{"type":"function_call","name":"lookup","arguments":"{}","call_id":"call-1"}`},
		{name: "output without call", items: `{"type":"function_call_output","call_id":"call-1","output":"result"}`},
		{name: "custom call without output", items: `{"type":"custom_tool_call","name":"apply_patch","input":"patch","call_id":"call-1"}`},
		{name: "custom output without call", items: `{"type":"custom_tool_call_output","name":"apply_patch","call_id":"call-1","output":"result"}`},
		{name: "shell call without output", items: `{"type":"local_shell_call","call_id":"call-1","action":{"type":"exec","command":"pwd"}}`},
		{name: "search call without output", items: `{"type":"tool_search_call","call_id":"call-1","arguments":{"query":"calendar"}}`},
		{name: "search output without call", items: `{"type":"tool_search_output","call_id":"call-1","tools":[]}`},
		{name: "duplicate calls", items: `{"type":"function_call","name":"one","arguments":"{}","call_id":"call-1"},{"type":"function_call","name":"two","arguments":"{}","call_id":"call-1"},{"type":"function_call_output","call_id":"call-1","output":"result"}`},
		{name: "duplicate outputs", items: `{"type":"function_call","name":"one","arguments":"{}","call_id":"call-1"},{"type":"function_call_output","call_id":"call-1","output":"one"},{"type":"function_call_output","call_id":"call-1","output":"two"}`},
		{name: "wrong output kind", items: `{"type":"function_call","name":"one","arguments":"{}","call_id":"call-1"},{"type":"custom_tool_call_output","call_id":"call-1","name":"one","output":"result"}`},
	}
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, FallbackContextWindowTokens: 0,
		MaxTokens: 10_000, ContextWindowFraction: 1, BytesPerToken: 1,
		RecentFraction: 0.9,
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-native","input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},` +
				testCase.items + `,` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}` +
				`]}`)
			transformed, transformer := PrepareRawResponsesCompaction(rawCompactionRequest(t, body), settings)
			if transformer != nil || !bytes.Equal(transformed.Body, body) {
				t.Fatalf("incomplete pair did not fail open: %s", transformed.Body)
			}
		})
	}
}

func TestRawResponsesCompactionRejectsDuplicateResponseFields(t *testing.T) {
	body := []byte(`{"model":"gpt-native","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"old user"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}
	]}`)
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	_, transformer := PrepareRawResponsesCompaction(rawCompactionRequest(t, body), settings)
	if transformer == nil {
		t.Fatal("expected a compaction transformer")
	}
	responseBody := []byte(`{"output":[],"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)
	got := readResponseBody(t, transformer.TransformResponse(rawJSONResponse(http.StatusOK, string(responseBody))))
	if !bytes.Equal(got, responseBody) {
		t.Fatalf("duplicate response fields were mutated: %s", got)
	}
}

func TestRawResponsesCompactionFailsOpenForMetadataAndMalformedRemovedItems(t *testing.T) {
	body := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"future_item","value":1},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.75,
	}
	raw := rawCompactionRequest(t, body)
	transformed, transformer := PrepareRawResponsesCompaction(raw, settings)
	if transformer != nil || !bytes.Equal(transformed.Body, body) {
		t.Fatalf("malformed removed item did not fail open: %s", transformed.Body)
	}

	raw.Header.Set(CodexTurnMetadataHeader, `{"session_id":"s","thread_source":"user","sandbox":"none","request_kind":"turn","compaction":{"implementation":"responses"}}`)
	transformed, transformer = PrepareRawResponsesCompaction(raw, settings)
	if transformer != nil || !bytes.Equal(transformed.Body, body) {
		t.Fatal("non-compaction metadata matched")
	}
	raw.Header.Set(CodexTurnMetadataHeader, `{"session_id":"s","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses_compact"}}`)
	transformed, transformer = PrepareRawResponsesCompaction(raw, settings)
	if transformer != nil || !bytes.Equal(transformed.Body, body) {
		t.Fatal("remote compaction implementation matched")
	}
}
