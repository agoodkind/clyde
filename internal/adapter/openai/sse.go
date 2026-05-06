package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"goodkind.io/clyde/internal/adapter/errcontract"
)

var ErrSSENoFlusher = errors.New("streaming not supported by this transport")

type SSEWriter struct {
	w                http.ResponseWriter
	f                http.Flusher
	headersCommitted bool
}

func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrSSENoFlusher
	}
	return &SSEWriter{w: w, f: f}, nil
}

func (sw *SSEWriter) WriteSSEHeaders() {
	if sw.headersCommitted {
		return
	}
	sw.w.Header().Set("Content-Type", "text/event-stream")
	sw.w.Header().Set("Cache-Control", "no-cache, no-transform")
	sw.w.Header().Set("Connection", "keep-alive")
	sw.w.Header().Set("X-Accel-Buffering", "no")
	sw.w.WriteHeader(http.StatusOK)
	sw.headersCommitted = true
	sw.f.Flush()
}

func (sw *SSEWriter) EmitStreamChunk(systemFingerprint string, chunk StreamChunk) error {
	sw.WriteSSEHeaders()
	chunk.SystemFingerprint = systemFingerprint
	b, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", b); err != nil {
		return err
	}
	sw.f.Flush()
	return nil
}

// WriteStreamEvent writes one SSE data frame and flushes it.
func (sw *SSEWriter) WriteStreamEvent(payload []byte) error {
	sw.WriteSSEHeaders()
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	sw.f.Flush()
	return nil
}

func (sw *SSEWriter) WriteStreamDone() error {
	if _, err := io.WriteString(sw.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	sw.f.Flush()
	return nil
}

// EmitStreamError writes an OpenAI-shaped native error envelope as a
// single SSE frame: `data: {"error": {"message": ..., "type": ...,
// "code": ...}}\n\n`. It does not write a `[DONE]` terminator;
// callers decide whether to follow with a finish chunk and DONE or
// terminate the stream as-is.
//
// Use this for upstream failures that should surface as a native
// error to the OpenAI client (Cursor, OpenAI SDK consumers) rather
// than as an assistant-shaped chat message.
func (sw *SSEWriter) EmitStreamError(info errcontract.ErrorInfo) error {
	renderer := NewStreamErrorRenderer()
	return renderer.emitErrorEvent(sw, info)
}

// StreamErrorRenderer renders OpenAI-family native error events on an
// already-open SSE stream. It owns only the error envelope; normal
// success stream chunks stay on SSEWriter.EmitStreamChunk.
type StreamErrorRenderer struct{}

// NewStreamErrorRenderer returns the canonical OpenAI stream error
// renderer.
func NewStreamErrorRenderer() StreamErrorRenderer { return StreamErrorRenderer{} }

// WriteStreamError emits one native OpenAI error event followed by
// the stream terminator.
func (r StreamErrorRenderer) WriteStreamError(w errcontract.StreamErrorWriter, info errcontract.ErrorInfo) error {
	if err := r.emitErrorEvent(w, info); err != nil {
		return err
	}
	if err := w.WriteStreamDone(); err != nil {
		slog.Warn("adapter.openai_stream_error_write_failed",
			"event", "write_done_failed",
			"err", err.Error(),
		)
		return fmt.Errorf("write stream done terminator: %w", err)
	}
	return nil
}

func (StreamErrorRenderer) emitErrorEvent(w errcontract.StreamErrorWriter, info errcontract.ErrorInfo) error {
	body := ErrorBody{
		Message: info.Message,
		Type:    info.Type,
		Code:    info.Code,
		Param:   info.Param,
	}
	body.Clyde = info.Diagnostics
	envelope := ErrorResponse{Error: body}
	b, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if err := w.WriteStreamEvent(b); err != nil {
		slog.Warn("adapter.openai_stream_error_write_failed",
			"event", "write_event_failed",
			"err", err.Error(),
		)
		return fmt.Errorf("write stream error event: %w", err)
	}
	return nil
}

func (sw *SSEWriter) HasCommittedHeaders() bool {
	return sw.headersCommitted
}
