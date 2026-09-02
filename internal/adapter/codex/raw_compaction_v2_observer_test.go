package codex

import (
	"bytes"
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

func TestRawResponsesCompactionV2SSEEncryptedContentRejectsInvalidOrderAndCardinality(t *testing.T) {
	compaction := `{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"cipher"}}`
	completed := `{"type":"response.completed","response":{"id":"resp-1"}}`
	for _, body := range []string{
		"data: " + completed + "\n\ndata: " + compaction + "\n\n",
		"data: " + compaction + "\n\ndata: " + completed + "\n\ndata: " + completed + "\n\n",
	} {
		if _, ok := rawResponsesCompactionV2SSEEncryptedContent([]byte(body)); ok {
			t.Fatal("invalid SSE sequence accepted")
		}
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
