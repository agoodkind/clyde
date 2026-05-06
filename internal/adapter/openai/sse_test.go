package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/errcontract"
)

type countingResponseRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *countingResponseRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

// TestEmitStreamErrorWritesNativeEnvelope locks in the OpenAI-shaped
// SSE error frame: `data: {"error":{"message":...,"type":...,"code":...}}\n\n`.
// Cursor and OpenAI SDK consumers consume this shape directly; the
// adapter must not synthesize it as an assistant message.
func TestEmitStreamErrorWritesNativeEnvelope(t *testing.T) {
	t.Parallel()

	rr := httptest.NewRecorder()
	sw, err := NewSSEWriter(rr)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}

	if err := sw.EmitStreamError(errcontract.ErrorInfo{
		Message: "anthropic 429: limit exceeded",
		Type:    "rate_limit_error",
		Code:    "rate_limit_exceeded",
		Param:   "messages",
	}); err != nil {
		t.Fatalf("EmitStreamError: %v", err)
	}

	body := rr.Body.String()
	if !strings.HasPrefix(body, "data: {") {
		t.Fatalf("envelope must start with `data: {`, got %q", body)
	}
	if !strings.Contains(body, `"error":{`) {
		t.Fatalf("envelope must contain native OpenAI error key, got %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("EmitStreamError must not write DONE terminator, got %q", body)
	}

	data := strings.TrimSuffix(strings.TrimPrefix(body, "data: "), "\n\n")
	var envelope ErrorResponse
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		t.Fatalf("unmarshal emitted envelope: %v", err)
	}
	if envelope.Error.Message != "anthropic 429: limit exceeded" {
		t.Fatalf("envelope must preserve message, got %q", envelope.Error.Message)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Fatalf("envelope must preserve type, got %q", envelope.Error.Type)
	}
	if envelope.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("envelope must preserve code, got %q", envelope.Error.Code)
	}
	if envelope.Error.Param != "messages" {
		t.Fatalf("envelope must preserve param, got %q", envelope.Error.Param)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("envelope must end with double newline, got %q", body)
	}
	if rr.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type must be text/event-stream, got %q", rr.Header().Get("Content-Type"))
	}
	if rr.Header().Get("Cache-Control") != "no-cache, no-transform" {
		t.Fatalf("Cache-Control must disable intermediary transforms, got %q", rr.Header().Get("Cache-Control"))
	}
	if rr.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering must disable proxy buffering, got %q", rr.Header().Get("X-Accel-Buffering"))
	}
}

func TestSSEWriterWriteHeadersIdempotent(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	sw.WriteSSEHeaders()
	sw.WriteSSEHeaders()
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
}

func TestSSEWriterFlushesEveryStreamChunk(t *testing.T) {
	rec := &countingResponseRecorder{ResponseRecorder: httptest.NewRecorder()}
	sw, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second"} {
		if err := sw.EmitStreamChunk("fp_test", StreamChunk{
			ID:      "req-stream",
			Object:  "chat.completion.chunk",
			Created: 1,
			Model:   "model",
			Choices: []StreamChoice{{
				Index: 0,
				Delta: StreamDelta{Content: text},
			}},
		}); err != nil {
			t.Fatalf("EmitStreamChunk: %v", err)
		}
	}
	if rec.flushes < 3 {
		t.Fatalf("flushes=%d want at least 3 for headers and two chunks", rec.flushes)
	}
}
