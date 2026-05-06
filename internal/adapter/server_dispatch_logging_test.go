package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
)

func TestHandleChatLogsSummaryBody(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode:  "summary",
			MaxKB: 32,
		},
	})

	body := map[string]any{
		"model":    "missing-model",
		"messages": []map[string]string{{"role": "system", "content": "ping"}},
	}
	postChatToServer(t, srv, body)

	rawEvt := findRawLogEvent(t, buf)
	if rawEvt == nil {
		t.Fatalf("expected adapter.chat.raw event")
	}
	if rawEvt["msg"] != "adapter.chat.raw" {
		t.Fatalf("msg = %q", rawEvt["msg"])
	}
	if _, hasBody := rawEvt["body"]; hasBody {
		t.Fatalf("summary mode should not include raw body")
	}
	if _, hasSummary := rawEvt["body_summary"]; !hasSummary {
		t.Fatalf("summary mode should include body_summary")
	}
}

func TestHandleChatLogsWhitelistBody(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode:  "whitelist",
			MaxKB: 4,
		},
	})

	body := map[string]any{
		"model": "missing-model",
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":       "weather",
					"parameters": map[string]any{"city": "string"},
				},
			},
		},
		"tool_choice": "auto",
		"messages": []map[string]any{
			{"role": "user", "content": strings.Repeat("x", 10000)},
		},
	}
	postChatToServer(t, srv, body)

	rawEvt := findRawLogEvent(t, buf)
	if rawEvt == nil {
		t.Fatalf("expected adapter.chat.raw event")
	}
	rawBody, ok := rawEvt["body"].(string)
	if !ok {
		t.Fatalf("body type = %T", rawEvt["body"])
	}
	if len(rawBody) > 4*1024 {
		t.Fatalf("whitelist body should be capped to 4KB")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(rawBody), &payload); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("expected tools in whitelist body")
	}
	tool0, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tool shape %T", tools[0])
	}
	if _, hasParams := tool0["parameters"]; hasParams {
		t.Fatalf("tool parameters should be redacted in whitelist body")
	}
	fn, ok := tool0["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool function shape = %T", tool0["function"])
	}
	if fn["name"] != "weather" {
		t.Fatalf("tool name = %v", fn["name"])
	}
	if _, ok := rawEvt["body_summary"]; !ok {
		t.Fatalf("whitelist mode should include body_summary")
	}
}

func TestHandleChatLogsRawBody(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode:  "raw",
			MaxKB: 1,
		},
	})

	body := map[string]any{
		"model":    "missing-model",
		"messages": []map[string]string{{"role": "system", "content": strings.Repeat("y", 2048)}},
	}
	postChatToServer(t, srv, body)

	rawEvt := findRawLogEvent(t, buf)
	if rawEvt == nil {
		t.Fatalf("expected adapter.chat.raw event")
	}
	rawBody, ok := rawEvt["body"].(string)
	if !ok {
		t.Fatalf("body type = %T", rawEvt["body"])
	}
	if len(rawBody) > 1024 {
		t.Fatalf("raw body should be capped to 1KB")
	}
	if truncated, ok := rawEvt["body_truncated"].(bool); !ok || !truncated {
		t.Fatalf("expected body_truncated=true")
	}
	if _, hasSummary := rawEvt["body_summary"]; !hasSummary {
		t.Fatalf("raw mode should include body_summary")
	}
	bodyB64, ok := rawEvt["body_b64"].(string)
	if !ok {
		t.Fatalf("raw mode should include body_b64")
	}
	decoded, err := base64.StdEncoding.DecodeString(bodyB64)
	if err != nil {
		t.Fatalf("decode body_b64: %v", err)
	}
	if len(decoded) > 1024 {
		t.Fatalf("decoded body_b64 should be capped to 1KB; got %d", len(decoded))
	}
}

func TestHandleChatUsesRuntimeBodyLoggingConfig(t *testing.T) {
	t.Parallel()
	runtimeLogging := NewRuntimeLogging(config.LoggingConfig{
		Body: config.LoggingBody{Mode: "summary", MaxKB: 32},
	})
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{Mode: "summary", MaxKB: 32},
	})
	srv.runtimeLogging = runtimeLogging

	body := map[string]any{
		"model":    "missing-model",
		"messages": []map[string]string{{"role": "user", "content": strings.Repeat("z", 2048)}},
	}
	postChatToServer(t, srv, body)
	summaryEvt := findRawLogEvent(t, buf)
	if summaryEvt == nil {
		t.Fatalf("expected summary adapter.chat.raw event")
	}
	if _, hasBody := summaryEvt["body"]; hasBody {
		t.Fatalf("summary mode should not include raw body")
	}

	buf.Reset()
	runtimeLogging.Set(config.LoggingConfig{
		Body: config.LoggingBody{Mode: "raw", MaxKB: 1},
	})
	postChatToServer(t, srv, body)
	rawEvt := findRawLogEvent(t, buf)
	if rawEvt == nil {
		t.Fatalf("expected raw adapter.chat.raw event")
	}
	if _, ok := rawEvt["body"].(string); !ok {
		t.Fatalf("raw mode should include body, got %T", rawEvt["body"])
	}
	if _, ok := rawEvt["body_b64"].(string); !ok {
		t.Fatalf("raw mode should include body_b64")
	}
	if truncated, ok := rawEvt["body_truncated"].(bool); !ok || !truncated {
		t.Fatalf("expected body_truncated=true after runtime max_kb change")
	}
}

func TestRequestDebugLogsRawPayloadForEveryRoute(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode:  "raw",
			MaxKB: 4,
		},
	})

	payload := `{"prompt":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Cursor/raw-debug-test")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	evt := findLogEvent(t, buf, "adapter.request.raw")
	if evt == nil {
		t.Fatalf("expected adapter.request.raw event")
	}
	if evt["path"] != "/v1/completions" {
		t.Fatalf("path=%v", evt["path"])
	}
	if evt["user_agent"] != "Cursor/raw-debug-test" {
		t.Fatalf("user_agent=%v", evt["user_agent"])
	}
	if evt["body"] != payload {
		t.Fatalf("body=%v", evt["body"])
	}
	if _, ok := evt["body_b64"].(string); !ok {
		t.Fatalf("expected body_b64 in raw request debug event")
	}
}

func TestAdapterErrorBoundaryRecoversPanicWithShapedError(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	})

	handler := srv.handle(adapterRouteOpenAI, func(context.Context, *handlerCtx) error {
		panic("boom")
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("User-Agent", "Cursor/panic-test")
	resp := httptest.NewRecorder()
	handler(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error response: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "internal_error" || out.Error.Code != "internal_error" {
		t.Fatalf("error response = %+v", out.Error)
	}
	if !strings.Contains(out.Error.Message, "request_id") {
		t.Fatalf("error message should include request_id: %q", out.Error.Message)
	}

	evt := findLogEvent(t, buf, "adapter.request.panic")
	if evt == nil {
		t.Fatalf("expected adapter.request.panic event")
	}
	if evt["path"] != "/v1/models" {
		t.Fatalf("path=%v", evt["path"])
	}
	if evt["response_started"] != false {
		t.Fatalf("response_started=%v", evt["response_started"])
	}
	if stack, ok := evt["stack"].(string); !ok || !strings.Contains(stack, "TestAdapterErrorBoundaryRecoversPanicWithShapedError") {
		t.Fatalf("stack missing test frame: %T %v", evt["stack"], evt["stack"])
	}
}

func TestHandleChatLogsOffModeSkipsEvent(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	})
	body := map[string]any{
		"model":    "missing-model",
		"messages": []map[string]string{{"role": "system", "content": "ping"}},
	}
	postChatToServer(t, srv, body)
	if evt := findRawLogEvent(t, buf); evt != nil {
		t.Fatalf("expected no adapter.chat.raw event in off mode")
	}
}

func TestHandleChatLogsIngressBeforeParse(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	req.Header.Set("User-Agent", "Cursor/route-probe")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	ingress := findLogEvent(t, buf, "adapter.chat.ingress")
	if ingress == nil {
		t.Fatalf("expected adapter.chat.ingress event")
	}
	if ingress["user_agent"] != "Cursor/route-probe" {
		t.Fatalf("user_agent=%v", ingress["user_agent"])
	}
	if ingress["body_bytes"] != float64(1) {
		t.Fatalf("body_bytes=%v", ingress["body_bytes"])
	}
	if evt := findLogEvent(t, buf, "adapter.chat.parse_failed"); evt == nil {
		t.Fatalf("expected adapter.chat.parse_failed event")
	}
}

func TestHandleChatLogsCorrelationFieldsAtBoundaries(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode:  "summary",
			MaxKB: 32,
		},
	}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.ModelPrefixes = []string{"gpt-"}
	})

	payload, err := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"metadata": map[string]string{
			"cursorRequestId":      "cursor-meta-req",
			"cursorConversationId": "cursor-meta-conv",
			"cursorGenerationId":   "cursor-meta-gen",
		},
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Clyde-Request-Id", "clyde-req-1")
	req.Header.Set("X-Clyde-Trace-Id", "0123456789abcdef0123456789abcdef")
	req.Header.Set("X-Clyde-Span-Id", "0123456789abcdef")
	req.Header.Set("X-Cursor-Request-Id", "cursor-header-req")
	req.Header.Set("X-Cursor-Conversation-Id", "cursor-header-conv")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	ingress := findLogEvent(t, buf, "adapter.chat.ingress")
	if ingress == nil {
		t.Fatalf("expected adapter.chat.ingress event")
	}
	assertCorrelationEvent(t, ingress, "clyde-req-1", "0123456789abcdef0123456789abcdef", "0123456789abcdef")
	if ingress["cursor_request_id"] != "cursor-header-req" {
		t.Fatalf("ingress cursor_request_id=%v", ingress["cursor_request_id"])
	}

	rawEvt := findLogEvent(t, buf, "adapter.chat.raw")
	if rawEvt == nil {
		t.Fatalf("expected adapter.chat.raw event")
	}
	assertCorrelationEvent(t, rawEvt, "clyde-req-1", "0123456789abcdef0123456789abcdef", "0123456789abcdef")

	received := findLogEvent(t, buf, "adapter.chat.received")
	if received == nil {
		t.Fatalf("expected adapter.chat.received event")
	}
	assertCorrelationEvent(t, received, "clyde-req-1", "0123456789abcdef0123456789abcdef", "0123456789abcdef")
	if received["cursor_request_id"] != "cursor-meta-req" {
		t.Fatalf("received cursor_request_id=%v", received["cursor_request_id"])
	}
	if received["cursor_conversation_id"] != "cursor-meta-conv" {
		t.Fatalf("received cursor_conversation_id=%v", received["cursor_conversation_id"])
	}
	if received["cursor_generation_id"] != "cursor-meta-gen" {
		t.Fatalf("received cursor_generation_id=%v", received["cursor_generation_id"])
	}
}

func assertCorrelationEvent(t *testing.T, evt map[string]any, requestID, traceID, parentSpanID string) {
	t.Helper()
	if evt["request_id"] != requestID {
		t.Fatalf("request_id=%v want %s", evt["request_id"], requestID)
	}
	if evt["trace_id"] != traceID {
		t.Fatalf("trace_id=%v want %s", evt["trace_id"], traceID)
	}
	if evt["parent_span_id"] != parentSpanID {
		t.Fatalf("parent_span_id=%v want %s", evt["parent_span_id"], parentSpanID)
	}
	if spanID, ok := evt["span_id"].(string); !ok || spanID == "" || spanID == parentSpanID {
		t.Fatalf("span_id=%v should be non-empty child span", evt["span_id"])
	}
}

func TestHandleChatAcceptsResponsesInputShape(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	})

	body := map[string]any{
		"model": "missing-model",
		"input": []map[string]any{
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "hello"},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)
	if strings.Contains(resp.Body.String(), "messages is required") {
		t.Fatalf("expected input normalization; got body %s", resp.Body.String())
	}
}

func TestHandleChatRejectsUnsupportedBackendWithoutLegacyRunner(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	})

	payload, err := json.Marshal(map[string]any{
		"model": "clyde-haiku-4-5",
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error response: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "invalid_request_error" || out.Error.Code != "unsupported_backend" {
		t.Fatalf("error = %+v body=%s", out.Error, resp.Body.String())
	}
}

func TestHandleChatLogsCursorModelNormalization(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.NativeModelRouting = "codex"
	})

	body := map[string]any{
		"model": "gpt-5.4",
		"reasoning": map[string]any{
			"effort":  "high",
			"summary": "auto",
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name": "Subagent",
				},
			},
		},
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	}
	postChatToServer(t, srv, body)

	evt := findLogEvent(t, buf, "adapter.chat.received")
	if evt == nil {
		t.Fatalf("expected adapter.chat.received event")
	}
	if evt["alias"] != "gpt-5.4" {
		t.Fatalf("alias=%v want gpt-5.4", evt["alias"])
	}
	if evt["cursor_raw_model"] != "gpt-5.4" {
		t.Fatalf("cursor_raw_model=%v", evt["cursor_raw_model"])
	}
	if evt["cursor_normalized_model"] != "gpt-5.4" {
		t.Fatalf("cursor_normalized_model=%v", evt["cursor_normalized_model"])
	}
	if evt["cursor_request_path"] != "foreground" {
		t.Fatalf("cursor_request_path=%v", evt["cursor_request_path"])
	}
	if evt["cursor_can_spawn_agent"] != true {
		t.Fatalf("cursor_can_spawn_agent=%v", evt["cursor_can_spawn_agent"])
	}
	resolved := findLogEvent(t, buf, "adapter.model.resolved")
	if resolved == nil {
		t.Fatalf("expected adapter.model.resolved event")
	}
	if resolved["backend"] != BackendCodex {
		t.Fatalf("resolved backend=%v", resolved["backend"])
	}
	dispatch := findLogEvent(t, buf, "adapter.backend.dispatching")
	if dispatch == nil {
		t.Fatalf("expected adapter.backend.dispatching event")
	}
	if dispatch["alias"] != "gpt-5.4" {
		t.Fatalf("dispatch alias=%v", dispatch["alias"])
	}
}

func TestHandleChatRoutesNativeCodexByDefaultWhenCodexEnabled(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.ModelPrefixes = []string{"gpt-", "o"}
	})

	body := map[string]any{
		"model": "gpt-5.4",
		"reasoning": map[string]any{
			"effort": "medium",
		},
		"input": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	}
	postChatToServer(t, srv, body)

	if evt := findLogEvent(t, buf, "adapter.model.resolve_failed"); evt != nil {
		t.Fatalf("did not expect model resolution failure: %v", evt)
	}
	evt := findLogEvent(t, buf, "adapter.chat.received")
	if evt == nil {
		t.Fatalf("expected adapter.chat.received event")
	}
	if evt["alias"] != "gpt-5.4" {
		t.Fatalf("alias=%v want gpt-5.4", evt["alias"])
	}
	if evt["backend"] != BackendCodex {
		t.Fatalf("backend=%v want %s", evt["backend"], BackendCodex)
	}
}

func TestHandleChatModelResolutionErrorUsesCursorNativeShape(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.NativeModelRouting = "off"
	})

	payload, err := json.Marshal(map[string]any{
		"model": "gpt-5.4",
		"input": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", resp.Code, resp.Body.String())
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error response: %v body=%s", err, resp.Body.String())
	}
	if out.Error.Type != "invalid_request_error" {
		t.Fatalf("error type = %q body=%s", out.Error.Type, resp.Body.String())
	}
	if out.Error.Code != "model_not_found" {
		t.Fatalf("error code = %q body=%s", out.Error.Code, resp.Body.String())
	}
	if out.Error.Param != "model" {
		t.Fatalf("error param = %q body=%s", out.Error.Param, resp.Body.String())
	}

	evt := findLogEvent(t, buf, "adapter.model.resolve_failed")
	if evt == nil {
		t.Fatalf("expected adapter.model.resolve_failed event")
	}
	if evt["model"] != "gpt-5.4" {
		t.Fatalf("model=%v want gpt-5.4", evt["model"])
	}
	if evt["cursor_normalized_model"] != "gpt-5.4" {
		t.Fatalf("cursor_normalized_model=%v", evt["cursor_normalized_model"])
	}
}

func TestServerAddrUsesIPv6LoopbackDefault(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{
		Body: config.LoggingBody{
			Mode: "off",
		},
	})

	if got := srv.Addr(); got != "[::1]:11434" {
		t.Fatalf("Addr()=%q want [::1]:11434", got)
	}
}

func newLoggingServer(t *testing.T, logging config.LoggingConfig, opts ...func(*config.AdapterConfig)) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg := baseConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	logBuffer := &bytes.Buffer{}
	srv, err := New(context.Background(), cfg, logging, Deps{
		ScratchDir: func() string { return t.TempDir() },
	}, slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logBuffer.Reset()
	return srv, logBuffer
}

func postChatToServer(t *testing.T, srv *Server, body map[string]any) {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)
}

func findRawLogEvent(t *testing.T, logBuffer *bytes.Buffer) map[string]any {
	return findLogEvent(t, logBuffer, "adapter.chat.raw")
}

func findLogEvent(t *testing.T, logBuffer *bytes.Buffer, message string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logBuffer.String()), "\n")
	for _, line := range slices.Backward(lines) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt["msg"] == message {
			return evt
		}
	}
	return nil
}
