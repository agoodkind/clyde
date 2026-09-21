package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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
	transcriptText := reorienttag.WrapInjection("recent", "")
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
	transcriptText := reorienttag.WrapInjection("recent", "")
	tests := []struct {
		name      string
		text      string
		unchanged bool
	}{
		{name: "opening tag", text: "quoted " + reorienttag.PreCompactionTranscriptOpen},
		{name: "closing tag", text: "quoted " + reorienttag.PreCompactionTranscriptClose},
		{name: "complete older wrapper", text: "summary" + reorienttag.WrapInjection("older", "")},
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

func TestRawResponsesCompactionEncodedReadFailurePreservesRemainingBody(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("create zstd encoder: %v", err)
	}
	original := encoder.EncodeAll([]byte(`{"output":[]}`), nil)
	if err := encoder.Close(); err != nil {
		t.Fatalf("close zstd encoder: %v", err)
	}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"zstd"}},
		Body: &partialReadCloser{
			prefix: original[:5],
			suffix: original[5:],
		},
	}
	got := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Equal(got, original) {
		t.Fatalf("partial encoded read body = %x, want %x", got, original)
	}
}

// rawResponseTransformerForTest is the transformer PrepareRawResponsesCompaction
// returns for a planned split: a wrapped injection with no mutation tracking.
func rawResponseTransformerForTest(t *testing.T) *RawResponsesCompactionTransformer {
	t.Helper()
	return &RawResponsesCompactionTransformer{
		injection:         reorienttag.WrapInjection("### User\n\nrecent\n\n### Assistant\n\nrecent assistant", ""),
		stream:            false,
		mutation:          nil,
		strictFinalAnswer: false,
	}
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

func rawFinalAnswerJSONResponse(status int, text string) *http.Response {
	body := []byte(`{"status":"completed","id":"resp-1","output":[{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":` + quotedJSONForTest(text) + `}]}]}`)
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
