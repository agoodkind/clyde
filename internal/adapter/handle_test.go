package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
)

// TestAdapterHandleAdapterErrorEnvelope verifies that when an adapterHandler
// returns an *adapterError, the boundary writes the matching shaped error
// envelope (OpenAI family for adapterRouteOpenAI here).
func TestAdapterHandleAdapterErrorEnvelope(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		return adapterErrInvalidRequest("missing field foo", nil)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := httptest.NewRecorder()
	handler(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "invalid_request_error" || out.Error.Code != "invalid_request" {
		t.Fatalf("error=%+v", out.Error)
	}
	if !strings.Contains(out.Error.Message, "missing field foo") {
		t.Fatalf("message=%q", out.Error.Message)
	}
}

// TestAdapterHandlePlainErrorWrapped verifies that a plain Go error returned
// from a handler is wrapped through adapterErrorFrom into the catch-all
// upstream_failed shape, so the response is always parsable.
func TestAdapterHandlePlainErrorWrapped(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		return errors.New("plain error from handler")
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	handler(resp, req)

	// Boundary remaps OpenAI-family non-2xx upstream into HTTP 400 +
	// invalid_request_error so Cursor BYOK does not fall back to its
	// generic rate-limit chrome. See AGENTS.md "Error boundary".
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type=%q", out.Error.Type)
	}
	if out.Error.Code != "upstream_failed" {
		t.Fatalf("error=%+v", out.Error)
	}
	if !strings.Contains(out.Error.Message, "plain error from handler") {
		t.Fatalf("message=%q", out.Error.Message)
	}
}

// TestAdapterHandleBodyNotWrittenBackstop verifies that a handler that
// returns nil without writing any bytes triggers the synthesized 502
// catch-all envelope rather than letting the client see an empty 200.
func TestAdapterHandleBodyNotWrittenBackstop(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	handler(resp, req)

	// Boundary remaps the synthesized backstop error into HTTP 400 +
	// invalid_request_error for OpenAI-family clients.
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type=%q", out.Error.Type)
	}
	if out.Error.Code != "upstream_failed" {
		t.Fatalf("error=%+v", out.Error)
	}
	if evt := findLogEvent(t, buf, "adapter.request.body_not_written"); evt == nil {
		t.Fatalf("expected adapter.request.body_not_written log event")
	}
}

// TestAdapterHandleRecoversPanic verifies that a panicking handler is
// recovered, the response shape matches the route family, and the panic is
// logged with stack and correlation fields.
func TestAdapterHandleRecoversPanic(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		panic("synthetic panic")
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	handler(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "internal_error" || out.Error.Code != "internal_error" {
		t.Fatalf("error=%+v", out.Error)
	}
	evt := findLogEvent(t, buf, "adapter.request.panic")
	if evt == nil {
		t.Fatalf("expected adapter.request.panic log event")
	}
	if evt["route_family"] != "openai" {
		t.Fatalf("route_family=%v", evt["route_family"])
	}
	if stack, _ := evt["stack"].(string); stack == "" {
		t.Fatalf("expected stack in panic log, got %v", evt["stack"])
	}
}

// TestAdapterHandlePartialResponseThenError verifies that when a handler
// commits headers (e.g. writes a 200 for a stream) and then returns an
// error, the boundary preserves the existing response and only logs the
// late error rather than double-writing an envelope on top.
func TestAdapterHandlePartialResponseThenError(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	handler := srv.handle(adapterRouteOpenAI, func(_ context.Context, hctx *handlerCtx) error {
		hctx.Writer.Header().Set("Content-Type", "application/json")
		hctx.Writer.WriteHeader(http.StatusOK)
		_, _ = hctx.Writer.Write([]byte(`{"partial":true}`))
		return errors.New("late stream error")
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := httptest.NewRecorder()
	handler(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"partial":true`) {
		t.Fatalf("body should retain partial response, got %s", resp.Body.String())
	}
	if evt := findLogEvent(t, buf, "adapter.request.error_after_headers"); evt == nil {
		t.Fatalf("expected adapter.request.error_after_headers log event")
	}
}
