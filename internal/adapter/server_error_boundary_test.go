package adapter

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

func TestAdapterErrorBoundaryPanicEnvelopeFollowsRouteFamily(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

	tests := []struct {
		name     string
		family   adapterRouteFamily
		path     string
		assertFn func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name:   "openai",
			family: adapterRouteOpenAI,
			path:   "/v1/chat/completions",
			assertFn: func(t *testing.T, resp *httptest.ResponseRecorder) {
				t.Helper()
				var out adapteropenai.ErrorResponse
				if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
					t.Fatalf("unmarshal OpenAI error: %v body=%s", err, resp.Body.String())
				}
				if out.Error.Type != "internal_error" || out.Error.Code != "internal_error" {
					t.Fatalf("OpenAI error = %+v", out.Error)
				}
			},
		},
		{
			name:   "anthropic",
			family: adapterRouteAnthropic,
			path:   "/v1/messages",
			assertFn: func(t *testing.T, resp *httptest.ResponseRecorder) {
				t.Helper()
				var out anthropic.ErrorEnvelope
				if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
					t.Fatalf("unmarshal Anthropic error: %v body=%s", err, resp.Body.String())
				}
				if out.Type != "error" || out.Error.Type != "api_error" {
					t.Fatalf("Anthropic error = %+v", out)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := srv.handle(tc.family, func(context.Context, *handlerCtx) error {
				panic("boundary probe")
			})
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			resp := httptest.NewRecorder()
			handler(resp, req)

			if resp.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			tc.assertFn(t, resp)
		})
	}
}

func TestAdapterErrorBoundaryLogsCorrelationFields(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		panic("correlated boundary probe")
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set(clydeingress.HeaderRequestID, "req-boundary-1")
	req.Header.Set(clydeingress.HeaderTraceID, "11111111111111111111111111111111")
	req.Header.Set(clydeingress.HeaderSpanID, "2222222222222222")
	req.Header.Set(adaptercursor.HeaderRequestID, "cursor-req-1")
	req.Header.Set(adaptercursor.HeaderConversationID, "cursor-conv-1")
	req.Header.Set("User-Agent", "Cursor/boundary-fields")
	resp := httptest.NewRecorder()
	handler(resp, req)

	if resp.Header().Get(clydeingress.HeaderRequestID) != "req-boundary-1" {
		t.Fatalf("response request id header=%q", resp.Header().Get(clydeingress.HeaderRequestID))
	}
	evt := findLogEvent(t, buf, "adapter.request.panic")
	if evt == nil {
		t.Fatalf("expected adapter.request.panic event")
	}
	if evt["request_id"] != "req-boundary-1" {
		t.Fatalf("request_id=%v", evt["request_id"])
	}
	if evt["trace_id"] != "11111111111111111111111111111111" {
		t.Fatalf("trace_id=%v", evt["trace_id"])
	}
	if evt["parent_span_id"] != "2222222222222222" {
		t.Fatalf("parent_span_id=%v", evt["parent_span_id"])
	}
	if evt["cursor_request_id"] != "cursor-req-1" {
		t.Fatalf("cursor_request_id=%v", evt["cursor_request_id"])
	}
	if evt["cursor_conversation_id"] != "cursor-conv-1" {
		t.Fatalf("cursor_conversation_id=%v", evt["cursor_conversation_id"])
	}
	if evt["method"] != http.MethodGet || evt["path"] != "/v1/models" || evt["user_agent"] != "Cursor/boundary-fields" {
		t.Fatalf("boundary route fields=%v", evt)
	}
}

func TestAdapterErrorBoundaryWritesLogHintConcernFile(t *testing.T) {
	root := t.TempDir()
	// SetupWithPolicy mutates the global slog default. Capture the
	// configured logger, restore the previous default immediately, and
	// pass the captured logger into New so a concurrent t.Parallel test in
	// this package never observes the mutated global default.
	previous := slog.Default()
	closer, err := slogger.SetupWithPolicy(slogger.SetupPolicy{
		Level: slog.LevelDebug,
		ProcessSink: slogger.FileSinkPolicy{
			Enabled: true,
			Path:    filepath.Join(root, "clyde-daemon.jsonl"),
			Rotation: slogger.RotationPolicy{
				Enabled:    false,
				MaxSizeMB:  0,
				MaxBackups: 0,
				MaxAgeDays: 0,
				Compress:   nil,
			},
		},
		ConcernRoot: filepath.Join(root, "logs"),
	})
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	policyLogger := slog.Default()
	slog.SetDefault(previous)
	t.Cleanup(func() { _ = closer.Close() })

	srv, err := New(context.Background(), baseConfig(), config.LoggingConfig{}, Deps{
		ScratchDir: func() string { return t.TempDir() },
	}, policyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		return adapterErrInvalidRequest("missing field foo", nil)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(clydeingress.HeaderRequestID, "req-log-hint-file")
	resp := httptest.NewRecorder()
	handler(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal OpenAI error: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Clyde == nil {
		t.Fatalf("missing diagnostics in body=%s", resp.Body.String())
	}
	if out.Error.Clyde.LogHint != "adapter/http/errors.jsonl request_id=req-log-hint-file" {
		t.Fatalf("log_hint=%q", out.Error.Clyde.LogHint)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close logger: %v", err)
	}

	errorsPath := filepath.Join(root, "logs", "adapter", "http", "errors.jsonl")
	content, err := os.ReadFile(errorsPath)
	if err != nil {
		t.Fatalf("read errors concern log %s: %v", errorsPath, err)
	}
	if !strings.Contains(string(content), "adapter.error.responded") {
		t.Fatalf("errors concern log missing adapter.error.responded: %s", content)
	}
	if !strings.Contains(string(content), "req-log-hint-file") {
		t.Fatalf("errors concern log missing request id: %s", content)
	}
}

func TestAdapterRequestPanicWritesErrorsConcernFile(t *testing.T) {
	root := t.TempDir()
	// See TestAdapterErrorBoundaryWritesLogHintConcernFile: restore the
	// global slog default immediately and thread the captured logger into
	// New so the mutation is never visible to a parallel sibling test.
	previous := slog.Default()
	closer, err := slogger.SetupWithPolicy(slogger.SetupPolicy{
		Level: slog.LevelDebug,
		ProcessSink: slogger.FileSinkPolicy{
			Enabled: true,
			Path:    filepath.Join(root, "clyde-daemon.jsonl"),
			Rotation: slogger.RotationPolicy{
				Enabled:    false,
				MaxSizeMB:  0,
				MaxBackups: 0,
				MaxAgeDays: 0,
				Compress:   nil,
			},
		},
		ConcernRoot: filepath.Join(root, "logs"),
	})
	if err != nil {
		t.Fatalf("SetupWithPolicy: %v", err)
	}
	policyLogger := slog.Default()
	slog.SetDefault(previous)
	t.Cleanup(func() { _ = closer.Close() })

	srv, err := New(context.Background(), baseConfig(), config.LoggingConfig{}, Deps{
		ScratchDir: func() string { return t.TempDir() },
	}, policyLogger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		panic("panic concern file probe")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(clydeingress.HeaderRequestID, "req-panic-file")
	resp := httptest.NewRecorder()
	handler(resp, req)
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("close logger: %v", err)
	}

	errorsPath := filepath.Join(root, "logs", "adapter", "http", "errors.jsonl")
	content, err := os.ReadFile(errorsPath)
	if err != nil {
		t.Fatalf("read errors concern log %s: %v", errorsPath, err)
	}
	if !strings.Contains(string(content), "adapter.request.panic") {
		t.Fatalf("errors concern log missing adapter.request.panic: %s", content)
	}
	if !strings.Contains(string(content), "req-panic-file") {
		t.Fatalf("errors concern log missing request id: %s", content)
	}
}

func TestAdapterAuthErrorEnvelopeFollowsRouteFamily(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.RequireToken = "secret-token"
	})

	openAIReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	openAIResp := httptest.NewRecorder()
	srv.mux.ServeHTTP(openAIResp, openAIReq)
	if openAIResp.Code != http.StatusUnauthorized {
		t.Fatalf("OpenAI status=%d body=%s", openAIResp.Code, openAIResp.Body.String())
	}
	var openAIOut adapteropenai.ErrorResponse
	if err := json.Unmarshal(openAIResp.Body.Bytes(), &openAIOut); err != nil {
		t.Fatalf("unmarshal OpenAI auth error: %v body=%s", err, openAIResp.Body.String())
	}
	if openAIOut.Error.Type != "authentication_error" || openAIOut.Error.Code != "unauthorized" {
		t.Fatalf("OpenAI auth error=%+v", openAIOut.Error)
	}

	anthropicReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	anthropicResp := httptest.NewRecorder()
	srv.mux.ServeHTTP(anthropicResp, anthropicReq)
	if anthropicResp.Code != http.StatusUnauthorized {
		t.Fatalf("Anthropic status=%d body=%s", anthropicResp.Code, anthropicResp.Body.String())
	}
	var anthropicOut anthropic.ErrorEnvelope
	if err := json.Unmarshal(anthropicResp.Body.Bytes(), &anthropicOut); err != nil {
		t.Fatalf("unmarshal Anthropic auth error: %v body=%s", err, anthropicResp.Body.String())
	}
	if anthropicOut.Type != "error" || anthropicOut.Error.Type != "authentication_error" {
		t.Fatalf("Anthropic auth error=%+v", anthropicOut)
	}
}

func TestAdapterInvalidJSONEnvelopeFollowsRouteFamily(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{})

	openAIReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	openAIResp := httptest.NewRecorder()
	srv.mux.ServeHTTP(openAIResp, openAIReq)
	if openAIResp.Code != http.StatusBadRequest {
		t.Fatalf("OpenAI status=%d body=%s", openAIResp.Code, openAIResp.Body.String())
	}
	var openAIOut adapteropenai.ErrorResponse
	if err := json.Unmarshal(openAIResp.Body.Bytes(), &openAIOut); err != nil {
		t.Fatalf("unmarshal OpenAI invalid JSON error: %v body=%s", err, openAIResp.Body.String())
	}
	if openAIOut.Error.Type != "invalid_request_error" || openAIOut.Error.Code != "invalid_json" || !strings.Contains(openAIOut.Error.Message, "invalid JSON") {
		t.Fatalf("OpenAI invalid JSON error=%+v", openAIOut.Error)
	}

	anthropicReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{"))
	anthropicResp := httptest.NewRecorder()
	srv.mux.ServeHTTP(anthropicResp, anthropicReq)
	if anthropicResp.Code != http.StatusBadRequest {
		t.Fatalf("Anthropic status=%d body=%s", anthropicResp.Code, anthropicResp.Body.String())
	}
	var anthropicOut anthropic.ErrorEnvelope
	if err := json.Unmarshal(anthropicResp.Body.Bytes(), &anthropicOut); err != nil {
		t.Fatalf("unmarshal Anthropic invalid JSON error: %v body=%s", err, anthropicResp.Body.String())
	}
	if anthropicOut.Error.Type != "invalid_request_error" || !strings.Contains(anthropicOut.Error.Message, "invalid JSON") {
		t.Fatalf("Anthropic invalid JSON error=%+v", anthropicOut)
	}
}

func TestAnthropicMessagesModelErrorUsesNativeEnvelope(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Models["local-codex"] = config.AdapterModelDeclaration{
			Provider:  config.AdapterModelProviderCodex,
			WireModel: "gpt-5.4",
			Profile:   "haiku",
		}
		cfg.Codex.Enabled = true
	})
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"local-codex","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`))
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var out anthropic.ErrorEnvelope
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal Anthropic model error: %v body=%s", err, resp.Body.String())
	}
	if out.Type != "error" || out.Error.Type != "invalid_request_error" {
		t.Fatalf("Anthropic model error=%+v", out)
	}
	if !strings.Contains(out.Error.Message, "anthropic backend") {
		t.Fatalf("message=%q", out.Error.Message)
	}
}
