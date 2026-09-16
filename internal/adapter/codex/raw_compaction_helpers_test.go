package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/gklog/correlation"
)

type rawCompactionSSEDecodedFrame struct {
	Type           string          `json:"type"`
	SequenceNumber int             `json:"sequence_number"`
	Delta          string          `json:"delta"`
	Item           json.RawMessage `json:"item"`
	Response       json.RawMessage `json:"response"`
}

type partialReadCloser struct {
	prefix []byte
	suffix []byte
	failed bool
}

func (r *partialReadCloser) Read(target []byte) (int, error) {
	if !r.failed {
		r.failed = true
		count := copy(target, r.prefix)
		return count, io.ErrUnexpectedEOF
	}
	if len(r.suffix) == 0 {
		return 0, io.EOF
	}
	count := copy(target, r.suffix)
	r.suffix = r.suffix[count:]
	return count, nil
}

func (r *partialReadCloser) Close() error {
	return nil
}

func rawCompactionSSEDecodedFramesForTest(t *testing.T, body []byte) []rawCompactionSSEDecodedFrame {
	t.Helper()
	frames := make([]rawCompactionSSEDecodedFrame, 0)
	for _, rawFrame := range bytes.Split(bytes.TrimSpace(body), []byte("\n\n")) {
		_, data, dataCount := rawSSEFrameDataValue(rawFrame)
		if dataCount != 1 {
			t.Fatalf("data fields = %d", dataCount)
		}
		var frame rawCompactionSSEDecodedFrame
		if json.Unmarshal(data, &frame) != nil {
			t.Fatalf("invalid frame: %s", rawFrame)
		}
		frames = append(frames, frame)
	}
	return frames
}

func rawCompactionSSEOutputTextForTest(t *testing.T, item json.RawMessage) string {
	t.Helper()
	var decoded struct {
		Content []rawCompactionSSEContentPart `json:"content"`
	}
	if json.Unmarshal(item, &decoded) != nil {
		t.Fatalf("invalid item: %s", item)
	}
	var text string
	for _, part := range decoded.Content {
		if part.Type == "output_text" {
			text += part.Text
		}
	}
	return text
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
		{name: "complete older wrapper", text: "summary" + wrappedRawCompactionTranscript("older")},
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

func TestSelectRawCompactionStartRejectsInvalidTargetCount(t *testing.T) {
	units := []rawCompactionInterval{{start: 0, end: 1}}
	for _, targetCount := range []int{0, 2} {
		selected, rendered, ok := selectRawCompactionStart(units, 1, targetCount, func(int) (string, bool) {
			t.Fatal("render called for invalid target count")
			return "", false
		})
		if ok || selected != 0 || rendered != "" {
			t.Fatalf("target count %d selected=%d rendered=%q ok=%t", targetCount, selected, rendered, ok)
		}
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
		{name: "null output text", status: http.StatusOK, contentType: "application/json", body: []byte(`{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":null}]}]}`)},
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

func TestRawResponsesCompactionReadFailurePreservesRemainingBody(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	original := []byte(`{"output":[]}`)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body: &partialReadCloser{
			prefix: original[:5],
			suffix: original[5:],
		},
	}
	got := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Equal(got, original) {
		t.Fatalf("partial read body = %q, want %q", got, original)
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

func rawCompactionSSEFramesForTest(item string, outputIndex int, itemSequence int, completedSequence int) (string, string) {
	itemDone := fmt.Sprintf(
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"sequence_number\":%d,\"item\":%s}\n\n",
		outputIndex,
		itemSequence,
		item,
	)
	completed := fmt.Sprintf(
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":%d,\"response\":{\"id\":\"resp-1\",\"output\":[%s]}}\n\n",
		completedSequence,
		item,
	)
	return itemDone, completed
}

func rawCompactionSuccessfulSSEFramesForTest(item string, outputIndex int, itemSequence int, completedSequence int) (string, string) {
	itemDone, completed := rawCompactionSSEFramesForTest(item, outputIndex, itemSequence, completedSequence)
	completed = strings.Replace(completed, `"response":{"id":`, `"response":{"status":"completed","id":`, 1)
	return itemDone, completed
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
