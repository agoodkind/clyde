package codex

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorienttag"
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

func TestPlanRawResponsesCompactionRendersDeveloperMessages(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"older user"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"older answer"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"recent developer instruction"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent answer"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}
	]}`)
	transformed, transformer := PrepareRawResponsesCompaction(
		rawCompactionRequest(t, body),
		RawResponsesCompactionSettings{
			Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
			ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
		},
	)
	if transformer == nil || bytes.Contains(transformed.Body, []byte("recent developer instruction")) {
		t.Fatal("v1 compaction did not remove the developer-containing recent turn")
	}
	responseBody := readResponseBody(t, transformer.TransformResponse(rawJSONResponse(http.StatusOK, "summary")))
	for _, content := range []string{"recent user", "recent developer instruction", "recent answer"} {
		if !bytes.Contains(responseBody, []byte(content)) {
			t.Fatalf("v1 recovery omitted %q", content)
		}
	}
}

func TestHasRawResponsesCompactionItem(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want bool
	}{
		{name: "compaction", body: `{"input":[{"type":"compaction","encrypted_content":"cipher"}]}`, want: true},
		{name: "message", body: `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`, want: false},
		{name: "malformed", body: `{`, want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := HasRawResponsesCompactionItem(RawResponsesRequest{Body: []byte(testCase.body)})
			if got != testCase.want {
				t.Fatalf("has compaction = %t, want %t", got, testCase.want)
			}
		})
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

func TestPlanRawResponsesCompactionHandlesLargeTranscriptWithinWatchdog(t *testing.T) {
	const itemCount = 10_001
	var body strings.Builder
	body.WriteString(`{"input":[`)
	for index := 0; index < itemCount; index++ {
		if index > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}`)
	}
	body.WriteString(`,{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	items := rawInputItemsForTest(t, []byte(body.String()))
	result := make(chan bool, 1)
	go func() {
		_, ok := planRawResponsesCompaction(items, 0, 1)
		result <- ok
	}()
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("large transcript did not produce a compaction plan")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("large transcript planning exceeded watchdog")
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

func TestRawResponsesCompactionRejectsDuplicateCallIDsAcrossTurns(t *testing.T) {
	body := []byte(`{"model":"gpt-native","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"old user"}]},
		{"type":"function_call","name":"old","arguments":"{}","call_id":"call-1"},
		{"type":"function_call_output","call_id":"call-1","output":"old result"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},
		{"type":"function_call","name":"recent","arguments":"{}","call_id":"call-1"},
		{"type":"function_call_output","call_id":"call-1","output":"recent result"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent assistant"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}
	]}`)
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	transformed, transformer := PrepareRawResponsesCompaction(rawCompactionRequest(t, body), settings)
	if transformer != nil || !bytes.Equal(transformed.Body, body) {
		t.Fatalf("duplicate call IDs did not fail open: %s", transformed.Body)
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

func TestRawResponsesCompactionV2RecoveryResponseTargetsFinalAnswer(t *testing.T) {
	recovery := &RawResponsesCompactionV2Recovery{transcript: "recovered transcript"}
	finalRequest := RawResponsesRequest{Header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"turn","compaction":{"phase":"final_answer"}}`}}}
	transformer := NewRawResponsesCompactionV2FinalAnswerTransformer(finalRequest, recovery)
	if transformer == nil {
		t.Fatal("final answer did not create transformer")
	}
	body := readResponseBody(t, transformer.TransformResponse(rawJSONResponse(http.StatusOK, "answer")))
	if !transformer.DidMutateResponse() || bytes.Count(body, []byte("<pre-compaction-transcript>")) != 1 {
		t.Fatalf("final answer mutation = %t body=%s", transformer.DidMutateResponse(), body)
	}

	for _, testCase := range []struct {
		name     string
		metadata string
		response *http.Response
	}{
		{name: "commentary", metadata: `{"request_kind":"turn","compaction":{"phase":"commentary"}}`, response: rawJSONResponse(http.StatusOK, "comment")},
		{name: "tool only", metadata: `{"request_kind":"turn","compaction":{"phase":"final_answer"}}`, response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":[{"type":"function_call","call_id":"call"}]}`))}},
		{name: "malformed", metadata: `{"request_kind":"turn","compaction":{"phase":"final_answer"}}`, response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"output":[`))}},
		{name: "duplicate tag", metadata: `{"request_kind":"turn","compaction":{"phase":"final_answer"}}`, response: rawJSONResponse(http.StatusOK, "<pre-compaction-transcript>kept</pre-compaction-transcript>")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := RawResponsesRequest{Header: http.Header{CodexTurnMetadataHeader: {testCase.metadata}}}
			candidate := NewRawResponsesCompactionV2FinalAnswerTransformer(request, recovery)
			if testCase.name == "commentary" {
				if candidate != nil {
					t.Fatal("commentary created transformer")
				}
				return
			}
			got := readResponseBody(t, candidate.TransformResponse(testCase.response))
			if candidate.DidMutateResponse() || bytes.Contains(got, []byte("recovered transcript")) {
				t.Fatalf("%s mutated: %s", testCase.name, got)
			}
		})
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

func TestRawResponsesCompactionMutatesMultilineSSEDataFrames(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	itemDone := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"item\":\n" +
		"data: {\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n"
	completed := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":\n" +
		"data: {\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}]}}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if bytes.Count(body, []byte("<pre-compaction-transcript>")) != 2 {
		t.Fatalf("multiline SSE transcript count was not two: %s", body)
	}
	for _, frame := range bytes.Split(bytes.TrimSpace(body), []byte("\n\n")) {
		_, data, dataCount := rawSSEFrameDataValue(frame)
		if dataCount != 1 || !json.Valid(data) {
			t.Fatalf("mutated multiline SSE frame is invalid: %s", frame)
		}
	}
}

func TestRawResponsesCompactionPreservesInterveningSSEFrames(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	itemDone := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n"
	heartbeat := ": keepalive\n\n"
	intervening := "event: response.future\ndata: {\"type\":\"response.future\",\"opaque\":true}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + heartbeat + intervening + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if bytes.Count(body, []byte("<pre-compaction-transcript>")) != 1 {
		t.Fatalf("intervening SSE frames prevented transcript injection: %s", body)
	}
	itemIndex := bytes.Index(body, []byte("response.output_item.done"))
	heartbeatIndex := bytes.Index(body, []byte(heartbeat))
	interveningIndex := bytes.Index(body, []byte("response.future"))
	completedIndex := bytes.Index(body, []byte("response.completed"))
	if heartbeatIndex < 0 || itemIndex <= heartbeatIndex || interveningIndex <= itemIndex || completedIndex <= interveningIndex {
		t.Fatalf("intervening SSE frame order changed: %s", body)
	}
}

func TestRawResponsesCompactionForwardsHeartbeatAfterCandidate(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	itemDone := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n"
	heartbeat := ": keepalive\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"
	upstream, writer := io.Pipe()
	release := make(chan struct{})
	go func() {
		_, _ = io.WriteString(writer, itemDone)
		_, _ = io.WriteString(writer, heartbeat)
		<-release
		_, _ = io.WriteString(writer, completed)
		_ = writer.Close()
	}()
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       upstream,
	}
	body := transformer.TransformResponse(response).Body
	t.Cleanup(func() { _ = body.Close() })
	firstFrame := make([]byte, len(heartbeat))
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(body, firstFrame)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read heartbeat: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("heartbeat after candidate did not reach live reader")
	}
	if string(firstFrame) != heartbeat {
		t.Fatalf("first downstream frame = %q, want heartbeat", firstFrame)
	}
	close(release)
	remainder := readResponseBody(t, &http.Response{Body: body})
	if !bytes.Contains(remainder, []byte("<pre-compaction-transcript>")) {
		t.Fatalf("completed response lost transcript: %s", remainder)
	}
}

func TestRawResponsesCompactionTransformsCompressedResponses(t *testing.T) {
	jsonBody := []byte(`{"id":"resp-native","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`)
	sseBody := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	for _, testCase := range []struct {
		name     string
		encoding string
		body     []byte
		stream   bool
	}{
		{name: "gzip JSON", encoding: "gzip", body: jsonBody},
		{name: "brotli JSON", encoding: "br", body: jsonBody},
		{name: "gzip SSE", encoding: "gzip", body: sseBody, stream: true},
		{name: "brotli SSE", encoding: "br", body: sseBody, stream: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			compressed := rawCompactionCompressedBodyForTest(t, testCase.body, testCase.encoding)
			contentType := "application/json"
			if testCase.stream {
				contentType = "text/event-stream"
			}
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Encoding": {testCase.encoding},
					"Content-Type":     {contentType},
				},
				Body: io.NopCloser(bytes.NewReader(compressed)),
			}
			transformer := rawResponseTransformerForTest(t)
			transformer.stream = testCase.stream
			transformed := transformer.TransformResponse(response)
			if got := transformed.Header.Get("Content-Encoding"); got != testCase.encoding {
				t.Fatalf("content encoding = %q, want %q", got, testCase.encoding)
			}
			decoded := rawCompactionDecompressedBodyForTest(t, readResponseBody(t, transformed), testCase.encoding)
			if !bytes.Contains(decoded, []byte("<pre-compaction-transcript>")) {
				t.Fatalf("compressed response lost transcript: %s", decoded)
			}
		})
	}
}

func rawCompactionCompressedBodyForTest(t *testing.T, body []byte, encoding string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&buffer)
	case "br":
		writer = brotli.NewWriter(&buffer)
	default:
		t.Fatalf("unsupported encoding %q", encoding)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("compress %s body: %v", encoding, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close %s writer: %v", encoding, err)
	}
	return buffer.Bytes()
}

func rawCompactionDecompressedBodyForTest(t *testing.T, body []byte, encoding string) []byte {
	t.Helper()
	var reader io.Reader
	switch encoding {
	case "gzip":
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("open gzip body: %v", err)
		}
		t.Cleanup(func() { _ = gzipReader.Close() })
		reader = gzipReader
	case "br":
		reader = brotli.NewReader(bytes.NewReader(body))
	default:
		t.Fatalf("unsupported encoding %q", encoding)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress %s body: %v", encoding, err)
	}
	return decoded
}

func TestRawResponsesCompactionMutatesOnlyFinalAssistantSSEItem(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	first := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"first assistant\"}]}}\n\n"
	final := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"final assistant\"}]}}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(first + final + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Contains(body, []byte(first)) {
		t.Fatalf("first assistant item changed: %s", body)
	}
	if bytes.Count(body, []byte("<pre-compaction-transcript>")) != 1 {
		t.Fatalf("transcript tag count was not one: %s", body)
	}
	finalStart := bytes.Index(body, []byte("final assistant"))
	tagStart := bytes.Index(body, []byte("<pre-compaction-transcript>"))
	if finalStart < 0 || tagStart < finalStart {
		t.Fatalf("final assistant did not receive transcript: %s", body)
	}
}

func TestRawResponsesCompactionKeepsCompletedSSEOutputCoherent(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	itemDone := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"No\"}]}}\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"refusal\",\"refusal\":\"No\"}]}]}}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if bytes.Count(body, []byte("<pre-compaction-transcript>")) != 2 {
		t.Fatalf("transcript was not appended to both SSE results: %s", body)
	}
	for _, frame := range bytes.Split(bytes.TrimSpace(body), []byte("\n\n")) {
		_, data, dataCount := rawSSEFrameDataValue(frame)
		if dataCount != 1 || !json.Valid(data) {
			t.Fatalf("invalid SSE frame: %s", frame)
		}
	}
}

func TestAppendRawCompactionAssistantItemCreatesOutputTextTarget(t *testing.T) {
	transcriptText := wrappedRawCompactionTranscript("recent")
	item := []byte(`{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"No"}]}`)
	mutated, matched, valid := appendRawCompactionAssistantItem(item, transcriptText)
	if !matched || !valid {
		t.Fatalf("fallback target result matched=%t valid=%t", matched, valid)
	}
	var decoded struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(mutated, &decoded); err != nil {
		t.Fatalf("decode fallback item: %v", err)
	}
	if len(decoded.Content) != 2 || decoded.Content[1].Type != "output_text" || decoded.Content[1].Text != transcriptText {
		t.Fatalf("fallback target = %#v", decoded.Content)
	}
}

func TestAppendRawCompactionAssistantItemRequiresCompleteTranscriptWrapper(t *testing.T) {
	transcriptText := wrappedRawCompactionTranscript("recent")
	tests := []struct {
		name      string
		text      string
		unchanged bool
	}{
		{name: "opening tag", text: "quoted " + reorienttag.PreCompactionTranscriptOpen},
		{name: "closing tag", text: "quoted " + reorienttag.PreCompactionTranscriptClose},
		{name: "complete current wrapper", text: "summary" + transcriptText, unchanged: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			item := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + quotedJSONForTest(testCase.text) + `}]}`)
			mutated, matched, valid := appendRawCompactionAssistantItem(item, transcriptText)
			if !matched || !valid {
				t.Fatalf("append result matched=%t valid=%t", matched, valid)
			}
			if testCase.unchanged {
				if !bytes.Equal(mutated, item) {
					t.Fatalf("complete wrapper changed:\n got: %s\nwant: %s", mutated, item)
				}
				return
			}
			var decoded struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(mutated, &decoded); err != nil {
				t.Fatalf("decode mutated item: %v", err)
			}
			if len(decoded.Content) != 1 || !strings.Contains(decoded.Content[0].Text, transcriptText) {
				t.Fatalf("lone tag prevented transcript injection: %s", mutated)
			}
		})
	}
}

func TestSelectRawCompactionStartUsesLogarithmicRenders(t *testing.T) {
	const unitCount = 1024
	units := make([]rawCompactionInterval, unitCount)
	for index := range units {
		units[index] = rawCompactionInterval{start: index, end: index + 1}
	}
	renderCount := 0
	selected, rendered, ok := selectRawCompactionStart(
		units,
		unitCount/2,
		unitCount,
		func(start int) (string, bool) {
			renderCount++
			return strings.Repeat("x", unitCount-start), true
		},
	)
	if !ok || selected != unitCount/2 || len(rendered) != unitCount/2 {
		t.Fatalf("selection selected=%d bytes=%d ok=%t", selected, len(rendered), ok)
	}
	if renderCount > rawCompactionLogarithmicRenderLimit(unitCount) {
		t.Fatalf("render count = %d, want logarithmic bound <= %d", renderCount, rawCompactionLogarithmicRenderLimit(unitCount))
	}
}

func rawCompactionLogarithmicRenderLimit(count int) int {
	limit := 1
	for size := 1; size < count; size *= 2 {
		limit++
	}
	return limit
}

func TestRawResponsesCompactionResponseFailuresPassThrough(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	assistantCandidate := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n"
	tests := []struct {
		name        string
		status      int
		contentType string
		body        []byte
	}{
		{name: "upstream failure", status: http.StatusBadRequest, contentType: "application/json", body: []byte(`{"error":"unchanged"}`)},
		{name: "malformed json", status: http.StatusOK, contentType: "application/json", body: []byte(`{"output":[`)},
		{name: "malformed sse item", status: http.StatusOK, contentType: "text/event-stream", body: []byte("event: response.output_item.done\ndata: {not-json}\n\n")},
		{name: "eof after candidate", status: http.StatusOK, contentType: "text/event-stream", body: []byte(assistantCandidate)},
		{name: "eof after later unknown frame", status: http.StatusOK, contentType: "text/event-stream", body: []byte(assistantCandidate + "event: response.future\ndata: {\"type\":\"response.future\",\"opaque\":true}\n\n")},
		{name: "stream response error after candidate", status: http.StatusOK, contentType: "text/event-stream", body: []byte(assistantCandidate + "event: response.failed\ndata: {\"type\":\"response.failed\",\"error\":{\"message\":\"failed\"}}\n\n")},
		{name: "malformed stream after candidate", status: http.StatusOK, contentType: "text/event-stream", body: []byte(assistantCandidate + "event: response.output_item.done\ndata: {not-json}\n\n")},
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
	body := []byte(`{"model":"gpt-native","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
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
	body := []byte(`{"id":"resp-1","status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + quotedJSONForTest(text) + `}],"opaque":true}],"unknown":{"keep":true}}`)
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
