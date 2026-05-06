package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/correlation"
)

func TestAdapterErrorBoundaryPanicEnvelopeFollowsRouteFamily(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{Mode: "off"},
	})

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
				var out ErrorResponse
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
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{Mode: "off"},
	})

	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		panic("correlated boundary probe")
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set(correlation.HeaderRequestID, "req-boundary-1")
	req.Header.Set(correlation.HeaderTraceID, "11111111111111111111111111111111")
	req.Header.Set(correlation.HeaderSpanID, "2222222222222222")
	req.Header.Set(correlation.HeaderCursorRequestID, "cursor-req-1")
	req.Header.Set(correlation.HeaderCursorConversationID, "cursor-conv-1")
	req.Header.Set("User-Agent", "Cursor/boundary-fields")
	resp := httptest.NewRecorder()
	handler(resp, req)

	if resp.Header().Get(correlation.HeaderRequestID) != "req-boundary-1" {
		t.Fatalf("response request id header=%q", resp.Header().Get(correlation.HeaderRequestID))
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

func TestAdapterAuthErrorEnvelopeFollowsRouteFamily(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{Mode: "off"},
	}, func(cfg *config.AdapterConfig) {
		cfg.RequireToken = "secret-token"
	})

	openAIReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	openAIResp := httptest.NewRecorder()
	srv.mux.ServeHTTP(openAIResp, openAIReq)
	if openAIResp.Code != http.StatusUnauthorized {
		t.Fatalf("OpenAI status=%d body=%s", openAIResp.Code, openAIResp.Body.String())
	}
	var openAIOut ErrorResponse
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
	srv, _ := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{Mode: "off"},
	})
	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{})

	openAIReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	openAIResp := httptest.NewRecorder()
	srv.mux.ServeHTTP(openAIResp, openAIReq)
	if openAIResp.Code != http.StatusBadRequest {
		t.Fatalf("OpenAI status=%d body=%s", openAIResp.Code, openAIResp.Body.String())
	}
	var openAIOut ErrorResponse
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
	srv, _ := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{Mode: "off"},
	}, func(cfg *config.AdapterConfig) {
		cfg.Models = map[string]config.AdapterModel{
			"local-codex": {
				Backend: BackendCodex,
				Model:   "gpt-5.4",
			},
		}
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
