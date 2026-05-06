package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	sw.WriteSSEHeaders()
	body := ErrorBody{
		Message: info.Message,
		Type:    info.Type,
		Code:    info.Code,
		Param:   info.Param,
	}
	envelope := ErrorResponse{Error: body}
	b, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(sw.w, "data: %s\n\n", b); err != nil {
		return err
	}
	sw.f.Flush()
	return nil
}

func (sw *SSEWriter) HasCommittedHeaders() bool {
	return sw.headersCommitted
}
