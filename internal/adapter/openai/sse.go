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

// ErrSSENoFlusher is part of Clyde's typed adapter surface.
var ErrSSENoFlusher = errors.New("streaming not supported by this transport")

// SSEWriter satisfies errcontract.StreamErrorWriter so the generic
// adapter error boundary can hand it mid-stream errors through the
// neutral primitive interface without importing this OpenAI package.
var _ errcontract.StreamErrorWriter = (*SSEWriter)(nil)

// SSEWriter is part of Clyde's typed adapter surface.
type SSEWriter struct {
	w                http.ResponseWriter
	f                http.Flusher
	headersCommitted bool
}

// NewSSEWriter is part of Clyde's typed adapter surface.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrSSENoFlusher
	}
	return &SSEWriter{w: w, f: f, headersCommitted:

	// WriteSSEHeaders is part of Clyde's typed adapter surface.
	false}, nil
}

// WriteSSEHeaders is part of Clyde's typed adapter surface.
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

// EmitStreamChunk is part of Clyde's typed adapter surface.
func (sw *SSEWriter) EmitStreamChunk(systemFingerprint string, chunk StreamChunk) error {
	sw.WriteSSEHeaders()
	chunk.SystemFingerprint = systemFingerprint
	b, err := json.Marshal(chunk)
	if err != nil {
		slog.Warn("adapter.openai_sse.marshal_chunk_failed", "concern", "adapter.chat.render", "err", err)
		return fmt.Errorf("marshal OpenAI stream chunk: %w", err)
	}
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", b); err != nil {
		slog.Warn("adapter.openai_sse.write_chunk_failed", "concern", "adapter.chat.render", "err", err)
		return fmt.Errorf("write OpenAI stream chunk: %w", err)
	}
	sw.f.Flush()
	return nil
}

// WriteStreamEvent writes one SSE data frame and flushes it.
func (sw *SSEWriter) WriteStreamEvent(payload []byte) error {
	sw.WriteSSEHeaders()
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", payload); err != nil {
		slog.Warn("adapter.openai_sse.write_event_failed", "concern", "adapter.chat.render", "err", err)
		return fmt.Errorf("write OpenAI stream event: %w", err)
	}
	sw.f.Flush()
	return nil
}

// WriteNamedEvent writes one named SSE frame as `event: <name>\ndata:
// <payload>\n\n` and flushes it. The OpenAI Responses stream uses named
// events, unlike the chat-completions stream that carries only unnamed
// data frames. The Responses path never calls WriteStreamDone, so no
// `[DONE]` terminator follows these frames.
func (sw *SSEWriter) WriteNamedEvent(name string, payload []byte) error {
	sw.WriteSSEHeaders()
	if _, err := fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", name, payload); err != nil {
		slog.Warn("adapter.openai_sse.write_named_event_failed", "concern", "adapter.chat.render", "event", name, "err", err)
		return fmt.Errorf("write OpenAI named event %q: %w", name, err)
	}
	sw.f.Flush()
	return nil
}

// WriteStreamDone is part of Clyde's typed adapter surface.
func (sw *SSEWriter) WriteStreamDone() error {
	if _, err := io.WriteString(sw.w, "data: [DONE]\n\n"); err != nil {
		slog.Warn("adapter.openai_sse.write_done_failed", "concern", "adapter.chat.render", "err", err)
		return fmt.Errorf("write OpenAI stream done: %w", err)
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
		slog.Warn("adapter.openai_stream_error_write_failed", "concern", "adapter.http.errors", "event", "write_done_failed",
			"err", err.Error(),
		)
		return fmt.Errorf("write stream done terminator: %w", err)
	}
	return nil
}

func (StreamErrorRenderer) emitErrorEvent(w errcontract.StreamErrorWriter, info errcontract.ErrorInfo) error {
	envelopeType := info.Type
	if envelopeType == "" {
		envelopeType = openAITypeForClass(info.Class)
	}
	body := ErrorBody{
		Message: info.Message,
		Type:    envelopeType,
		Code:    info.Code,
		Param:   info.Param,
		Clyde:   info.Diagnostics,
	}
	envelope := ErrorResponse{Error: body}
	b, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal OpenAI stream error: %w", err)
	}
	if err := w.WriteStreamEvent(b); err != nil {
		slog.Warn("adapter.openai_stream_error_write_failed", "concern", "adapter.http.errors", "event", "write_event_failed",
			"err", err.Error(),
		)
		return fmt.Errorf("write stream error event: %w", err)
	}
	return nil
}

// HasCommittedHeaders is part of Clyde's typed adapter surface.
func (sw *SSEWriter) HasCommittedHeaders() bool {
	return sw.headersCommitted
}
