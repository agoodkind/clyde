package codex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestObserveRawResponsesCompactionV2ResponsePreservesAndArms(t *testing.T) {
	for _, testCase := range []struct {
		name, encoding string
		body           []byte
	}{
		{name: "json", body: []byte(`{"output":[{"type":"compaction","encrypted_content":"cipher"}]}`)},
		{name: "sse", body: []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := NewRawResponsesCompactionV2Registry(nil)
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "X-Test": {"kept"}}, Body: io.NopCloser(bytes.NewReader(testCase.body))}
			if testCase.name == "sse" {
				response.Header.Set("Content-Type", "text/event-stream")
			}
			observed := ObserveRawResponsesCompactionV2Response(response, RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"}, registry)
			got, err := io.ReadAll(observed.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, testCase.body) || observed.Header.Get("X-Test") != "kept" {
				t.Fatalf("client bytes or header changed")
			}
			ArmRawResponsesCompactionV2Response(observed)
			if transcript, ok := registry.Match("s", "cipher"); !ok || transcript != "t" {
				t.Fatal("recovery not armed")
			}
		})
	}
}

func TestObserveRawResponsesCompactionV2ResponseAcceptsAllSSELineEndings(t *testing.T) {
	compaction := `{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"cipher"}}`
	completed := `{"type":"response.completed","response":{"id":"resp-1"}}`
	for _, testCase := range []struct {
		name       string
		lineEnding string
		separator  string
	}{
		{name: "lf", lineEnding: "\n", separator: "\n\n"},
		{name: "crlf", lineEnding: "\r\n", separator: "\r\n\r\n"},
		{name: "cr", lineEnding: "\r", separator: "\r\r"},
		{name: "mixed", lineEnding: "\r\n", separator: "\n\r"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte("event: response.output_item.done" + testCase.lineEnding + "data: " + compaction + testCase.separator +
				"event: response.completed" + testCase.lineEnding + "data: " + completed + testCase.separator)
			if encrypted, ok := rawResponsesCompactionV2SSEEncryptedContent(body); !ok || encrypted != "cipher" {
				t.Fatalf("full parser encrypted=%q matched=%t", encrypted, ok)
			}
			registry := NewRawResponsesCompactionV2Registry(nil)
			response := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       &oneByteReadCloser{reader: bytes.NewReader(body)},
			}
			observed := ObserveRawResponsesCompactionV2Response(
				response,
				RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"},
				registry,
			)
			got, err := io.ReadAll(observed.Body)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatalf("client body changed: err=%v body=%q", err, got)
			}
			ArmRawResponsesCompactionV2Response(observed)
			if transcript, ok := registry.Match("s", "cipher"); !ok || transcript != "t" {
				t.Fatal("recovery not armed")
			}
		})
	}
}

func TestRawResponsesCompactionV2SSEFrameScanAdvancesLinearly(t *testing.T) {
	body := []byte(`data: {"padding":"` + strings.Repeat("x", 128*1024) + `"}` + "\r\n\r\n")
	buffer := make([]byte, 0, len(body))
	scanOffset := 0
	scanWork := 0
	completed := false
	for _, value := range body {
		buffer = append(buffer, value)
		scanWork += len(buffer) - scanOffset
		_, remainder, complete, nextScanOffset := rawResponsesCompactionV2SSEFrameFrom(buffer, scanOffset, false)
		if complete {
			buffer = remainder
			scanOffset = 0
			completed = true
			continue
		}
		scanOffset = nextScanOffset
	}
	if !completed || len(buffer) != 0 {
		t.Fatal("incremental scanner did not finish the frame")
	}
	if scanWork > len(body)*4 {
		t.Fatalf("scan work = %d, want at most %d", scanWork, len(body)*4)
	}
}

func TestObserveRawResponsesCompactionV2ResponseArmsBeforeEOF(t *testing.T) {
	body := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	observed := ObserveRawResponsesCompactionV2Response(
		response,
		RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"},
		registry,
	)
	destination := make([]byte, len(body))
	count, err := observed.Body.Read(destination)
	if err != nil || count != len(body) || !bytes.Equal(destination[:count], body) {
		t.Fatalf("first read changed: count=%d err=%v", count, err)
	}
	if transcriptText, ok := registry.Match("s", "cipher"); !ok || transcriptText != "t" {
		t.Fatal("terminal frame did not arm recovery before EOF")
	}
}

func TestObserveRawResponsesCompactionV2ResponseDisarmsInvalidPostTerminalTail(t *testing.T) {
	prefix := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	for _, tail := range [][]byte{
		[]byte(": post-terminal comment\n\n"),
		[]byte("data: {\"type\":\"response.future\"}\n\n"),
		[]byte("data: {"),
	} {
		registry := NewRawResponsesCompactionV2Registry(nil)
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       &chunkReadCloser{chunks: [][]byte{prefix, tail}},
		}
		observed := ObserveRawResponsesCompactionV2Response(
			response,
			RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"},
			registry,
		)
		first := make([]byte, len(prefix))
		count, err := observed.Body.Read(first)
		if err != nil || count != len(prefix) || !bytes.Equal(first, prefix) {
			t.Fatalf("terminal read changed: count=%d err=%v", count, err)
		}
		if _, ok := registry.Match("s", "cipher"); !ok {
			t.Fatal("terminal frame did not arm recovery")
		}
		remaining, err := io.ReadAll(observed.Body)
		if err != nil || !bytes.Equal(remaining, tail) {
			t.Fatalf("tail read changed: err=%v body=%q", err, remaining)
		}
		if _, ok := registry.Match("s", "cipher"); ok {
			t.Fatalf("post-terminal tail %q kept recovery armed", tail)
		}
	}
}

func TestObserveRawResponsesCompactionV2ResponseDisarmsAfterReadError(t *testing.T) {
	prefix := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body: &chunkReadCloser{
			chunks:     [][]byte{prefix},
			finalError: errors.New("upstream read failed"),
		},
	}
	observed := ObserveRawResponsesCompactionV2Response(
		response,
		RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"},
		registry,
	)
	first := make([]byte, len(prefix))
	if _, err := observed.Body.Read(first); err != nil {
		t.Fatalf("terminal read: %v", err)
	}
	if _, ok := registry.Match("s", "cipher"); !ok {
		t.Fatal("terminal frame did not arm recovery")
	}
	if _, err := observed.Body.Read(first); err == nil || !strings.Contains(err.Error(), "upstream read failed") {
		t.Fatalf("read error = %v", err)
	}
	if _, ok := registry.Match("s", "cipher"); ok {
		t.Fatal("read error kept recovery armed")
	}
}

func TestReleaseRawResponsesCompactionV2ResponseDisarmsWithIncompleteTail(t *testing.T) {
	prefix := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	tail := []byte("data: {")
	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       &chunkReadCloser{chunks: [][]byte{prefix, tail}},
	}
	observed := ObserveRawResponsesCompactionV2Response(
		response,
		RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"},
		registry,
	)
	first := make([]byte, len(prefix))
	if _, err := observed.Body.Read(first); err != nil {
		t.Fatalf("terminal read: %v", err)
	}
	second := make([]byte, len(tail))
	if _, err := observed.Body.Read(second); err != nil || !bytes.Equal(second, tail) {
		t.Fatalf("tail read changed: err=%v body=%q", err, second)
	}
	if _, ok := registry.Match("s", "cipher"); !ok {
		t.Fatal("terminal frame did not arm recovery")
	}
	ReleaseRawResponsesCompactionV2Response(observed)
	if _, ok := registry.Match("s", "cipher"); ok {
		t.Fatal("release kept incomplete response recovery armed")
	}
}

type chunkReadCloser struct {
	chunks     [][]byte
	finalError error
}

func (r *chunkReadCloser) Read(destination []byte) (int, error) {
	if len(r.chunks) == 0 {
		if r.finalError != nil {
			return 0, r.finalError
		}
		return 0, io.EOF
	}
	count := copy(destination, r.chunks[0])
	r.chunks[0] = r.chunks[0][count:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return count, nil
}

func (r *chunkReadCloser) Close() error {
	return nil
}

type oneByteReadCloser struct {
	reader *bytes.Reader
}

func (r *oneByteReadCloser) Read(destination []byte) (int, error) {
	if len(destination) > 1 {
		destination = destination[:1]
	}
	count, err := r.reader.Read(destination)
	if errors.Is(err, io.EOF) {
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read one-byte fixture: %w", err)
	}
	return count, nil
}

func (r *oneByteReadCloser) Close() error {
	return nil
}

func TestRawResponsesCompactionV2SSEEncryptedContentAcceptsLargeMultilineData(t *testing.T) {
	cipher := strings.Repeat("x", 128*1024)
	body := []byte("data: {\"type\":\"response.output_item.done\",\n" +
		"data: \"item\":{\"type\":\"compaction\",\"encrypted_content\":\"" + cipher + "\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	got, ok := rawResponsesCompactionV2SSEEncryptedContent(body)
	if !ok || got != cipher {
		t.Fatalf("encrypted content matched=%t length=%d want %d", ok, len(got), len(cipher))
	}
}

func TestObserveRawResponsesCompactionV2ResponseBoundsZstdOutput(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	wire := encoder.EncodeAll(bytes.Repeat([]byte("x"), maxRawResponsesCompactionV2ObserveBytes+1), nil)
	_ = encoder.Close()
	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"zstd"}},
		Body:       io.NopCloser(bytes.NewReader(wire)),
	}
	observed := ObserveRawResponsesCompactionV2Response(response, RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"}, registry)
	got, err := io.ReadAll(observed.Body)
	if err != nil || !bytes.Equal(got, wire) {
		t.Fatal("zstd client bytes changed")
	}
	ArmRawResponsesCompactionV2Response(observed)
	if _, ok := registry.Match("s", "cipher"); ok {
		t.Fatal("oversized zstd output armed recovery")
	}
}

func TestObserveRawResponsesCompactionV2ResponseZstdAndFailures(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"output":[{"type":"compaction","encrypted_content":"cipher"}]}`)
	wire := encoder.EncodeAll(body, nil)
	_ = encoder.Close()
	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"zstd"}}, Body: io.NopCloser(bytes.NewReader(wire))}
	observed := ObserveRawResponsesCompactionV2Response(response, RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"}, registry)
	got, err := io.ReadAll(observed.Body)
	if err != nil || !bytes.Equal(got, wire) {
		t.Fatal("zstd client body changed")
	}
	ArmRawResponsesCompactionV2Response(observed)
	if _, ok := registry.Match("s", "cipher"); !ok {
		t.Fatal("zstd recovery not armed")
	}
	malformed := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader([]byte(`{`)))}
	observed = ObserveRawResponsesCompactionV2Response(malformed, RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"}, registry)
	_, _ = io.ReadAll(observed.Body)
	if _, ok := registry.Match("s", ""); ok {
		t.Fatal("malformed response armed state")
	}
}

func TestObserveRawResponsesCompactionV2ResponseRejectsIncompleteSSE(t *testing.T) {
	plain := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n")
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	zstdBody := encoder.EncodeAll(plain, nil)
	_ = encoder.Close()
	for _, testCase := range []struct {
		name, encoding string
		body           []byte
	}{
		{name: "plain", body: plain},
		{name: "zstd", encoding: "zstd", body: zstdBody},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry := NewRawResponsesCompactionV2Registry(nil)
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {testCase.encoding}}, Body: io.NopCloser(bytes.NewReader(testCase.body))}
			observed := ObserveRawResponsesCompactionV2Response(response, RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"}, registry)
			got, readErr := io.ReadAll(observed.Body)
			if readErr != nil || !bytes.Equal(got, testCase.body) {
				t.Fatal("client bytes changed")
			}
			ArmRawResponsesCompactionV2Response(observed)
			if _, ok := registry.Match("s", "cipher"); ok {
				t.Fatal("incomplete SSE armed recovery")
			}
		})
	}
}

func TestObserveRawResponsesCompactionV2ResponseRejectsTruncatedTailAfterCompletion(t *testing.T) {
	prefix := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	tail := []byte("data: {")
	body := append(append([]byte(nil), prefix...), tail...)
	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       &chunkReadCloser{chunks: [][]byte{prefix, tail}},
	}
	observed := ObserveRawResponsesCompactionV2Response(
		response,
		RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"},
		registry,
	)
	got, err := io.ReadAll(observed.Body)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("client body changed: err=%v body=%q", err, got)
	}
	ArmRawResponsesCompactionV2Response(observed)
	if _, ok := registry.Match("s", "cipher"); ok {
		t.Fatal("truncated SSE tail armed recovery")
	}
}

func TestObserveRawResponsesCompactionV2ResponseRejectsTailBeyondCaptureLimit(t *testing.T) {
	prefix := []byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\n\n")
	comment := []byte(": padding\n\n")
	tail := bytes.Repeat(comment, maxRawResponsesCompactionV2ObserveBytes/len(comment)+1)
	tail = append(tail, "data: {"...)
	body := append(append([]byte(nil), prefix...), tail...)
	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       &chunkReadCloser{chunks: [][]byte{prefix, tail}},
	}
	observed := ObserveRawResponsesCompactionV2Response(
		response,
		RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"},
		registry,
	)
	got, err := io.ReadAll(observed.Body)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("client body changed: err=%v length=%d", err, len(got))
	}
	ArmRawResponsesCompactionV2Response(observed)
	if _, ok := registry.Match("s", "cipher"); ok {
		t.Fatal("capture-truncated SSE tail armed recovery")
	}
}

func TestRawResponsesCompactionV2SSEEncryptedContentRejectsInvalidOrderAndCardinality(t *testing.T) {
	compaction := `{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"cipher"}}`
	completed := `{"type":"response.completed","response":{"id":"resp-1"}}`
	for _, body := range []string{
		"data: " + completed + "\n\ndata: " + compaction + "\n\n",
		"data: " + compaction + "\n\ndata: " + completed + "\n\ndata: " + completed + "\n\n",
		"data: " + compaction + "\n\ndata: " + completed + "\n\n: post-terminal comment\n\n",
		"data: " + compaction + "\n\ndata: " + completed + "\n\ndata: {\"type\":\"response.future\"}\n\n",
	} {
		if _, ok := rawResponsesCompactionV2SSEEncryptedContent([]byte(body)); ok {
			t.Fatal("invalid SSE sequence accepted")
		}
	}
}

func TestRawResponsesCompactionV2SSEEncryptedContentRejectsUnterminatedFrame(t *testing.T) {
	body := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}")
	if _, ok := rawResponsesCompactionV2SSEEncryptedContent(body); ok {
		t.Fatal("unterminated completed frame was accepted")
	}
}

func TestObserveRawResponsesCompactionV2ResponseRejectsInvalidSSESequences(t *testing.T) {
	compaction := `{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"cipher"}}`
	completed := `{"type":"response.completed","response":{"id":"resp-1"}}`
	for _, sequence := range []string{
		"data: " + completed + "\n\ndata: " + compaction + "\n\n",
		"data: " + compaction + "\n\ndata: " + completed + "\n\ndata: " + completed + "\n\n",
	} {
		encoder, err := zstd.NewWriter(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, testCase := range []struct {
			body     []byte
			encoding string
		}{{body: []byte(sequence)}, {body: encoder.EncodeAll([]byte(sequence), nil), encoding: "zstd"}} {
			registry := NewRawResponsesCompactionV2Registry(nil)
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {testCase.encoding}}, Body: io.NopCloser(bytes.NewReader(testCase.body))}
			observed := ObserveRawResponsesCompactionV2Response(response, RawResponsesCompactionV2Plan{SessionID: "s", Transcript: "t"}, registry)
			_, _ = io.ReadAll(observed.Body)
			ArmRawResponsesCompactionV2Response(observed)
			if _, ok := registry.Match("s", "cipher"); ok {
				t.Fatal("invalid sequence armed recovery")
			}
		}
		_ = encoder.Close()
	}
}
