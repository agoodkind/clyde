package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/gklog/correlation"
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
	lastMessage, ok := renderRawResponsesCompactionItems(items[5:6])
	if !ok {
		t.Fatal("last message did not render")
	}
	capPlan, ok := planRawResponsesCompaction(items, len(lastMessage), 0.5)
	if !ok || capPlan.removedStart != 5 {
		t.Fatalf("cap plan = %+v ok=%t, want only index 5 removed", capPlan, ok)
	}
	underCap, ok := planRawResponsesCompaction(items, len(lastMessage)-1, 0.5)
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
				testCase.call + `,` + testCase.output + `,` +
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

func TestRawResponsesCompactionMutatesNonStreamingJSONOnce(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	response := rawJSONResponse(http.StatusOK, "summary")
	response.Header.Set("X-Opaque", "kept")
	transformed := transformer.TransformResponse(response)
	body := readResponseBody(t, transformed)
	if transformed.Header.Get("X-Opaque") != "kept" || transformed.Header.Get("Content-Length") != "" {
		t.Fatalf("response headers = %v", transformed.Header)
	}
	if bytes.Count(body, []byte("<pre-compaction-transcript>")) != 1 || !bytes.Contains(body, []byte("recent")) {
		t.Fatalf("response was not injected once: %s", body)
	}

	repeated := transformer.TransformResponse(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	})
	repeatedBody := readResponseBody(t, repeated)
	if !bytes.Equal(repeatedBody, body) {
		t.Fatalf("repeated transformation changed response:\n got: %s\nwant: %s", repeatedBody, body)
	}
}

func TestRawResponsesCompactionMutatesStreamingItemAndPreservesUnknownFrames(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	unknownFrame := "event: response.future\n: keep this exact comment\ndata: { \"opaque\" : [1, 2] }\n\n"
	itemDone := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"
	original := []byte(unknownFrame + itemDone + completed)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "Content-Length": {"1"}},
		Body:       io.NopCloser(bytes.NewReader(original)),
	}
	transformed := transformer.TransformResponse(response)
	body := readResponseBody(t, transformed)
	if !bytes.Contains(body, []byte(unknownFrame)) {
		t.Fatalf("unknown SSE frame changed:\n%s", body)
	}
	if bytes.Count(body, []byte("<pre-compaction-transcript>")) != 1 || !bytes.Contains(body, []byte("recent")) {
		t.Fatalf("streaming response was not injected once: %s", body)
	}
	if transformed.Header.Get("Content-Length") != "" {
		t.Fatalf("content length survived mutation: %v", transformed.Header)
	}
}

func TestRawResponsesCompactionResponseFailuresPassThrough(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	tests := []struct {
		name        string
		status      int
		contentType string
		body        []byte
	}{
		{name: "upstream failure", status: http.StatusBadRequest, contentType: "application/json", body: []byte(`{"error":"unchanged"}`)},
		{name: "malformed json", status: http.StatusOK, contentType: "application/json", body: []byte(`{"output":[`)},
		{name: "malformed sse item", status: http.StatusOK, contentType: "text/event-stream", body: []byte("event: response.output_item.done\ndata: {not-json}\n\n")},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode: testCase.status,
				Header:     http.Header{"Content-Type": {testCase.contentType}, "X-Opaque": {"kept"}},
				Body:       io.NopCloser(bytes.NewReader(testCase.body)),
			}
			transformed := transformer.TransformResponse(response)
			got := readResponseBody(t, transformed)
			if !bytes.Equal(got, testCase.body) || transformed.Header.Get("X-Opaque") != "kept" {
				t.Fatalf("failure did not pass through:\n got: %s\nwant: %s", got, testCase.body)
			}
		})
	}
}

func rawCompactionRequest(t *testing.T, body []byte) RawResponsesRequest {
	t.Helper()
	return RawResponsesRequest{
		Body:      body,
		Header:    http.Header{CodexTurnMetadataHeader: {`{"session_id":"s","thread_source":"user","sandbox":"none","request_kind":"compaction","compaction":{"implementation":"responses"}}`}},
		RequestID: "req", Correlation: correlation.Context{}, Stream: false,
	}
}

func rawResponseTransformerForTest(t *testing.T) *RawResponsesCompactionTransformer {
	t.Helper()
	body := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	_, transformer := PrepareRawResponsesCompaction(
		rawCompactionRequest(t, body),
		RawResponsesCompactionSettings{
			Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
			ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
		},
	)
	if transformer == nil {
		t.Fatal("expected transformer")
	}
	return transformer
}

func rawJSONResponse(status int, text string) *http.Response {
	body := []byte(`{"id":"resp-1","output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + quotedJSONForTest(text) + `}],"opaque":true}],"unknown":{"keep":true}}`)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}, "Content-Length": {"1"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func rawInputItemsForTest(t *testing.T, body []byte) []json.RawMessage {
	t.Helper()
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return request.Input
}

func quotedJSONForTest(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func readResponseBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return body
}
