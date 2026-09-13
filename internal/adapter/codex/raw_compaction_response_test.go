package codex

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

func TestRawResponsesCompactionMutatesNonStreamingJSONOnce(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	response := rawJSONResponse(http.StatusOK, "summary")
	response.Header.Set("X-Opaque", "kept")
	response.Header.Set("ETag", "stale")
	response.Header.Set("Digest", "stale")
	response.Header.Set("Content-Digest", "stale")
	transformed := transformer.TransformResponse(response)
	body := readResponseBody(t, transformed)
	if transformed.Header.Get("X-Opaque") != "kept" || transformed.Header.Get("Content-Length") != "" ||
		transformed.Header.Get("ETag") != "" || transformed.Header.Get("Digest") != "" || transformed.Header.Get("Content-Digest") != "" {
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
	itemDone, completed := rawCompactionSSEFramesForTest(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}`, 0, 10, 11)
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
	if !bytes.Contains(body, []byte("<pre-compaction-transcript>")) || !bytes.Contains(body, []byte("recent")) {
		t.Fatalf("streaming response was not injected once: %s", body)
	}
	if transformed.Header.Get("Content-Length") != "" {
		t.Fatalf("content length survived mutation: %v", transformed.Header)
	}
}

func TestRawResponsesCompactionMutatesMultilineSSEDataFrames(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	itemDone := "event: response.output_item.done\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"sequence_number\":10,\"item\":\n" +
		"data: {\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}}\n\n"
	completed := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"sequence_number\":11,\"response\":\n" +
		"data: {\"id\":\"resp-1\",\"output\":[{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"summary\"}]}]}}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Contains(body, []byte("<pre-compaction-transcript>")) {
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
	itemDone, completed := rawCompactionSSEFramesForTest(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}`, 0, 10, 12)
	heartbeat := ": keepalive\n\n"
	intervening := "event: response.future\ndata: {\"type\":\"response.future\",\"sequence_number\":11,\"opaque\":true}\n\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(itemDone + heartbeat + intervening + completed)),
	}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Contains(body, []byte("<pre-compaction-transcript>")) {
		t.Fatalf("intervening SSE frames prevented transcript injection: %s", body)
	}
	itemIndex := bytes.Index(body, []byte("response.output_item.done"))
	heartbeatIndex := bytes.Index(body, []byte(heartbeat))
	interveningIndex := bytes.Index(body, []byte("response.future"))
	completedIndex := bytes.Index(body, []byte("response.completed"))
	if itemIndex < 0 || heartbeatIndex <= itemIndex || interveningIndex <= heartbeatIndex || completedIndex <= interveningIndex {
		t.Fatalf("intervening SSE frame order changed: %s", body)
	}
}

func TestRawResponsesCompactionPreservesUnknownTextSSEFrame(t *testing.T) {
	transformer := rawResponseTransformerForTest(t)
	transformer.stream = true
	itemDone := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}}\n\n"
	unknown := "event: extension\ndata: ping\n\n"
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(itemDone + unknown + completed))}
	body := readResponseBody(t, transformer.TransformResponse(response))
	if !bytes.Equal(body, []byte(itemDone+unknown+completed)) {
		t.Fatalf("unknown text SSE frame disrupted compaction: %s", body)
	}
}

func TestRawSSEFrameEventUsesLastField(t *testing.T) {
	frame := []byte("event: response.future\nevent: response.completed\n\n")
	if event := rawSSEFrameEvent(frame); event != rawCompactionSSECompleted {
		t.Fatalf("event = %q, want %q", event, rawCompactionSSECompleted)
	}
}

func TestRawResponsesCompactionTransformsCompressedResponses(t *testing.T) {
	jsonBody := []byte(`{"id":"resp-native","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`)
	itemDone, completed := rawCompactionSSEFramesForTest(`{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}`, 0, 10, 11)
	sseBody := []byte(itemDone + completed)
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
