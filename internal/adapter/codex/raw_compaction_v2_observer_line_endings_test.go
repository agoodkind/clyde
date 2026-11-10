package codex

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

func TestObserveRawResponsesCompactionV2ResponseAcceptsStandaloneCarriageReturns(t *testing.T) {
	body := []byte("event: response.output_item.done\rdata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"cipher\"}}\r\r" +
		"event: response.completed\rdata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\"}}\r\r")
	if encrypted, ok := rawResponsesCompactionV2SSEEncryptedContent(body); !ok || encrypted != "cipher" {
		t.Fatalf("standalone carriage return encrypted=%q matched=%t", encrypted, ok)
	}

	registry := NewRawResponsesCompactionV2Registry(nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	observed := ObserveRawResponsesCompactionV2Response(
		response,
		RawResponsesCompactionV2Plan{SessionID: "session-1", Injection: "transcript"},
		registry,
	)
	got, err := io.ReadAll(observed.Body)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("client body changed: err=%v body=%q", err, got)
	}
	ArmRawResponsesCompactionV2Response(observed)
	if transcript, ok := registry.Match("session-1", "cipher"); !ok || transcript != "transcript" {
		t.Fatalf("standalone carriage return did not arm recovery: transcript=%q matched=%t", transcript, ok)
	}
}
