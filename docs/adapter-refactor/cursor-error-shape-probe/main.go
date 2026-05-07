// Package main runs a local OpenAI-compatible probe server for Cursor error
// envelope experiments.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultAddress = "localhost:48761"
	defaultShape   = "json-current-upstream-failed"
	serverTimeout  = 30 * time.Second
)

type errorShape struct {
	name       string
	status     int
	errorType  string
	errorCode  string
	streamMode streamMode
}

type shapeState struct {
	Name       string `json:"name"`
	Status     int    `json:"status"`
	ErrorType  string `json:"error_type"`
	ErrorCode  string `json:"error_code"`
	StreamMode string `json:"stream_mode,omitempty"`
}

type streamMode string

const (
	streamModeNone           streamMode = ""
	streamModeErrorDone      streamMode = "error_done"
	streamModeErrorOnly      streamMode = "error_only"
	streamModeDeltaErrorDone streamMode = "delta_error_done"
	streamModeEventErrorDone streamMode = "event_error_done"
	streamModeChunkErrorDone streamMode = "chunk_error_done"
)

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
	Param   string `json:"param,omitempty"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type modelsResponse struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

type streamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type probeServer struct {
	shape atomic.Value
}

func main() {
	address := flag.String("addr", defaultAddress, "listen address")
	initialShape := flag.String("shape", envDefault("CLYDE_CURSOR_ERROR_SHAPE", defaultShape), "initial error shape")
	flag.Parse()

	server := newProbeServer(*initialShape)
	mux := http.NewServeMux()
	mux.HandleFunc("/__probe__/shape", server.handleShape)
	mux.HandleFunc("/v1/models", server.handleModels)
	mux.HandleFunc("/v1/chat/completions", server.handleChatCompletions)

	slog.Info("cursor error-shape probe server listening",
		"address", *address,
		"shape", server.currentShape().name,
	)
	httpServer := &http.Server{
		Addr:              *address,
		Handler:           mux,
		ReadHeaderTimeout: serverTimeout,
		ReadTimeout:       serverTimeout,
		WriteTimeout:      serverTimeout,
		IdleTimeout:       serverTimeout,
	}
	if err := httpServer.ListenAndServe(); err != nil {
		slog.Error("cursor error-shape probe server failed", "err", err)
		os.Exit(1)
	}
}

func envDefault(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func newProbeServer(initialShape string) *probeServer {
	server := &probeServer{}
	shape, err := lookupShape(initialShape)
	if err != nil {
		slog.Warn("unknown initial shape, using default",
			"requested_shape", initialShape,
			"default_shape", defaultShape,
			"err", err,
		)
		shape, _ = lookupShape(defaultShape)
	}
	server.shape.Store(shape)
	return server
}

func (s *probeServer) currentShape() errorShape {
	shape, ok := s.shape.Load().(errorShape)
	if !ok {
		shape, _ = lookupShape(defaultShape)
		return shape
	}
	return shape
}

func (s *probeServer) handleShape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requestedShape := strings.TrimSpace(r.URL.Query().Get("shape"))
	if requestedShape != "" {
		shape, err := lookupShape(requestedShape)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.shape.Store(shape)
	}
	writeShapeJSON(w, http.StatusOK, s.currentShape())
}

func (s *probeServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeModelsJSON(w, http.StatusOK, modelsResponse{
		Object: "list",
		Data: []modelInfo{{
			ID:      "clyde-error-shape-probe",
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: "clyde-local-probe",
		}},
	})
}

func (s *probeServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	shape := s.currentShape()
	if shape.streamMode != streamModeNone {
		writeStreamShape(w, shape)
		return
	}
	writeErrorJSON(w, shape.status, buildErrorResponse(shape))
}

func writeStreamShape(w http.ResponseWriter, shape errorShape) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if shape.streamMode == streamModeDeltaErrorDone ||
		shape.streamMode == streamModeChunkErrorDone {
		writeSSEChunk(w, streamChunk{
			ID:      "chatcmpl-clyde-error-shape-probe",
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   "clyde-error-shape-probe",
			Choices: []streamChoice{{
				Index: 0,
				Delta: streamDelta{
					Role:    "assistant",
					Content: "probe pre-error delta",
				},
			}},
		})
	}

	switch shape.streamMode {
	case streamModeNone,
		streamModeErrorDone,
		streamModeErrorOnly,
		streamModeDeltaErrorDone,
		streamModeChunkErrorDone:
		writeSSE(w, buildErrorResponse(shape))
	case streamModeEventErrorDone:
		writeNamedSSE(w, "error", buildErrorResponse(shape))
	}
	if shape.streamMode != streamModeErrorOnly {
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func buildErrorResponse(shape errorShape) errorResponse {
	return errorResponse{
		Error: errorBody{
			Message: "clyde cursor error-shape probe: " + shape.name,
			Type:    shape.errorType,
			Code:    shape.errorCode,
		},
	}
}

func writeErrorJSON(w http.ResponseWriter, status int, payload errorResponse) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode JSON", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes)
}

func writeModelsJSON(w http.ResponseWriter, status int, payload modelsResponse) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode JSON", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes)
}

func writeShapeJSON(w http.ResponseWriter, status int, shape errorShape) {
	payload := shapeState{
		Name:       shape.name,
		Status:     shape.status,
		ErrorType:  shape.errorType,
		ErrorCode:  shape.errorCode,
		StreamMode: string(shape.streamMode),
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to encode JSON", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes)
}

func writeSSE(w http.ResponseWriter, payload errorResponse) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", bytes)
}

func writeSSEChunk(w http.ResponseWriter, payload streamChunk) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", bytes)
}

func writeNamedSSE(w http.ResponseWriter, eventName string, payload errorResponse) {
	bytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, bytes)
}

func lookupShape(name string) (errorShape, error) {
	for _, shape := range shapes() {
		if shape.name == name {
			return shape, nil
		}
	}
	return errorShape{}, fmt.Errorf("unknown shape %q", name)
}

func shapes() []errorShape {
	return []errorShape{
		{
			name:      "json-current-upstream-failed",
			status:    http.StatusBadRequest,
			errorType: "invalid_request_error",
			errorCode: "upstream_failed",
		},
		{
			name:      "json-upstream-unavailable",
			status:    http.StatusBadRequest,
			errorType: "invalid_request_error",
			errorCode: "upstream_unavailable",
		},
		{
			name:      "json-temporarily-unavailable",
			status:    http.StatusBadRequest,
			errorType: "invalid_request_error",
			errorCode: "temporarily_unavailable",
		},
		{
			name:      "json-503-server-error",
			status:    http.StatusServiceUnavailable,
			errorType: "server_error",
			errorCode: "upstream_failed",
		},
		{
			name:      "json-503-invalid-request",
			status:    http.StatusServiceUnavailable,
			errorType: "invalid_request_error",
			errorCode: "upstream_failed",
		},
		{
			name:       "sse-error-done-current",
			status:     http.StatusOK,
			errorType:  "invalid_request_error",
			errorCode:  "upstream_failed",
			streamMode: streamModeErrorDone,
		},
		{
			name:       "sse-error-only-current",
			status:     http.StatusOK,
			errorType:  "invalid_request_error",
			errorCode:  "upstream_failed",
			streamMode: streamModeErrorOnly,
		},
		{
			name:       "sse-delta-error-done-current",
			status:     http.StatusOK,
			errorType:  "invalid_request_error",
			errorCode:  "upstream_failed",
			streamMode: streamModeDeltaErrorDone,
		},
		{
			name:       "sse-event-error-done-current",
			status:     http.StatusOK,
			errorType:  "invalid_request_error",
			errorCode:  "upstream_failed",
			streamMode: streamModeEventErrorDone,
		},
	}
}
