package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
)

func TestHandleChatLogsFixedFilteredPayload(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	body := map[string]any{
		"model":       "missing-model",
		"temperature": 0.2,
		"messages":    []map[string]string{{"role": "system", "content": "ping"}},
	}
	postChatToServer(t, srv, body)

	payloadEvent := findPayloadLogEvent(t, buf)
	if payloadEvent == nil {
		t.Fatalf("expected adapter payload request leg")
	}
	if payloadEvent["msg"] != "logging.request.leg" {
		t.Fatalf("msg = %q", payloadEvent["msg"])
	}
	if _, hasBody := payloadEvent["body"]; hasBody {
		t.Fatalf("fixed payload policy should not include body")
	}
	if _, hasBodyB64 := payloadEvent["body_b64"]; hasBodyB64 {
		t.Fatalf("fixed payload policy should not include body_b64")
	}
	if _, hasBodySummary := payloadEvent["body_summary"]; hasBodySummary {
		t.Fatalf("fixed payload policy should not include legacy body_summary")
	}
	if _, hasSummary := payloadEvent["payload_summary"]; !hasSummary {
		t.Fatalf("fixed payload policy should include payload_summary")
	}
	if _, hasFields := payloadEvent["payload_fields"]; hasFields {
		t.Fatalf("payload_fields must not be logged; bodies live in capture.db: %v", payloadEvent["payload_fields"])
	}
	if _, hasRemoved := payloadEvent["payload_removed"]; hasRemoved {
		t.Fatalf("payload_removed must not be logged: %v", payloadEvent["payload_removed"])
	}
}

func TestHandleChatEmitsRequiredRequestLegSequence(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.Models = testCodexModels()
	})

	postChatToServer(t, srv, map[string]any{
		"model": "gpt-5.4",
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
	})

	seen := make(map[string]bool)
	for _, event := range findLogEvents(t, buf, "logging.request.leg") {
		if event["surface"] != string(logevent.SurfaceAdapterChat) {
			continue
		}
		leg, ok := event["leg"].(string)
		if ok {
			seen[leg] = true
		}
	}
	for _, leg := range logevent.DefaultRequiredLegs()[logevent.SurfaceAdapterChat] {
		if !seen[string(leg)] {
			t.Fatalf("missing required leg %s in events %v", leg, seen)
		}
	}
	if event := findLogEvent(t, buf, "logging.request.incomplete"); event != nil {
		t.Fatalf("did not expect incomplete request event: %v", event)
	}
}

func TestHandleChatEarlyFailureEmitsErrorLegWithoutIncomplete(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	errorLeg := false
	for _, event := range findLogEvents(t, buf, "logging.request.leg") {
		if event["leg"] == string(logevent.LegRequestError) {
			errorLeg = true
		}
	}
	if !errorLeg {
		t.Fatalf("expected request_error leg")
	}
	if event := findLogEvent(t, buf, "logging.request.incomplete"); event != nil {
		t.Fatalf("did not expect incomplete request event: %v", event)
	}
}

func TestAdapterRoutesDoNotEmitRequestDebugBypass(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	payload := `{"prompt":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	if evt := findLogEvent(t, buf, "logging.request.debug"); evt != nil {
		t.Fatalf("did not expect logging.request.debug bypass event: %v", evt)
	}
}

func TestAdapterErrorBoundaryRecoversPanicWithShapedError(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

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

func TestHandleChatLogsIngressBeforeParse(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{"))
	req.Header.Set("User-Agent", "Cursor/route-probe")
	resp := httptest.NewRecorder()
	srv.mux.ServeHTTP(resp, req)

	ingress := findRequestLegLogEvent(t, buf, logevent.LegAdapterIngress)
	if ingress == nil {
		t.Fatalf("expected adapter ingress request leg")
	}
	if ingress["phase"] != string(logevent.PhaseStarted) {
		t.Fatalf("phase=%v", ingress["phase"])
	}
	if ingress["status"] != string(logevent.StatusOK) {
		t.Fatalf("status=%v", ingress["status"])
	}
	if evt := findLogEvent(t, buf, "adapter.chat.parse_failed"); evt == nil {
		t.Fatalf("expected adapter.chat.parse_failed event")
	}
}

func TestHandleChatLogsCorrelationFieldsAtBoundaries(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.Models = testCodexModels()
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

	ingress := findRequestLegLogEvent(t, buf, logevent.LegAdapterIngress)
	if ingress == nil {
		t.Fatalf("expected adapter ingress request leg")
	}
	assertCorrelationEvent(t, ingress, "clyde-req-1", "0123456789abcdef0123456789abcdef", "0123456789abcdef")
	if cursorFacet := facetFromEvent(t, ingress, "cursor"); cursorFacet["request_id"] != "cursor-header-req" {
		t.Fatalf("ingress cursor.request_id=%v", cursorFacet["request_id"])
	}

	payloadEvent := findPayloadLogEvent(t, buf)
	if payloadEvent == nil {
		t.Fatalf("expected adapter payload request leg")
	}
	assertCorrelationEvent(t, payloadEvent, "clyde-req-1", "0123456789abcdef0123456789abcdef", "0123456789abcdef")

	received := findLogEvent(t, buf, "adapter.chat.received")
	if received == nil {
		t.Fatalf("expected adapter.chat.received event")
	}
	assertCorrelationEvent(t, received, "clyde-req-1", "0123456789abcdef0123456789abcdef", "0123456789abcdef")
	cursorFacet := facetFromEvent(t, received, "cursor")
	if cursorFacet["request_id"] != "cursor-meta-req" {
		t.Fatalf("received cursor.request_id=%v", cursorFacet["request_id"])
	}
	if cursorFacet["conversation_id"] != "cursor-meta-conv" {
		t.Fatalf("received cursor.conversation_id=%v", cursorFacet["conversation_id"])
	}
	if cursorFacet["generation_id"] != "cursor-meta-gen" {
		t.Fatalf("received cursor.generation_id=%v", cursorFacet["generation_id"])
	}
	if received["chat_key_source"] != "native" {
		t.Fatalf("received chat_key_source=%v", received["chat_key_source"])
	}
	if received["chat_root_key"] != "cursor-header-conv" {
		t.Fatalf("received chat_root_key=%v", received["chat_root_key"])
	}
}

func TestHandleChatAttachesRegisteredBackendFacet(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.Models = testCodexModels()
	})

	postChatToServer(t, srv, map[string]any{
		"model":        "gpt-5.4",
		"service_tier": "priority",
		"reasoning": map[string]string{
			"effort":  "high",
			"summary": "auto",
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name": "Search",
				},
			},
		},
		"messages": []map[string]string{{"role": "user", "content": "ping"}},
	})

	sendStarted := findRequestLegLogEvent(t, buf, logevent.LegProviderSendStarted)
	if sendStarted == nil {
		t.Fatalf("expected provider_send_started request leg")
	}
	codexFacet := facetFromEvent(t, sendStarted, "codex")
	if codexFacet["model"] != "gpt-5.4" {
		t.Fatalf("codex.model=%v", codexFacet["model"])
	}
	if codexFacet["effort"] != "high" {
		t.Fatalf("codex.effort=%v", codexFacet["effort"])
	}
	if codexFacet["reasoning_summary"] != "auto" {
		t.Fatalf("codex.reasoning_summary=%v", codexFacet["reasoning_summary"])
	}
	if codexFacet["service_tier"] != "priority" {
		t.Fatalf("codex.service_tier=%v", codexFacet["service_tier"])
	}
	if codexFacet["input_count"] != float64(1) {
		t.Fatalf("codex.input_count=%v", codexFacet["input_count"])
	}
	if codexFacet["tool_count"] != float64(1) {
		t.Fatalf("codex.tool_count=%v", codexFacet["tool_count"])
	}
}

func TestHandleChatLogsForkDetectedForDerivedBranch(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.Models = testCodexModels()
	})

	postChatToServer(t, srv, map[string]any{
		"model": "gpt-5.4",
		"messages": []map[string]string{
			{"role": "user", "content": "same first prompt"},
			{"role": "assistant", "content": "assistant one"},
			{"role": "user", "content": "continue left"},
		},
	})
	postChatToServer(t, srv, map[string]any{
		"model": "gpt-5.4",
		"messages": []map[string]string{
			{"role": "user", "content": "same first prompt"},
			{"role": "assistant", "content": "assistant one"},
			{"role": "user", "content": "continue right"},
		},
	})

	fork := findLogEvent(t, buf, "adapter.chat.fork_detected")
	if fork == nil {
		t.Fatalf("expected adapter.chat.fork_detected event")
	}
	if fork["chat_key_source"] != "derived" {
		t.Fatalf("fork chat_key_source=%v", fork["chat_key_source"])
	}
	if fork["chat_branch_key"] != "b02" {
		t.Fatalf("fork chat_branch_key=%v", fork["chat_branch_key"])
	}
	if fork["prior_branch_key"] != "b01" {
		t.Fatalf("fork prior_branch_key=%v", fork["prior_branch_key"])
	}
	if fork["message_count"] != float64(3) {
		t.Fatalf("fork message_count=%v", fork["message_count"])
	}
	if fork["tool_count"] != float64(0) {
		t.Fatalf("fork tool_count=%v", fork["tool_count"])
	}
	if fork["common_prefix_len"] != float64(2) {
		t.Fatalf("fork common_prefix_len=%v", fork["common_prefix_len"])
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
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

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

// TestHandleChatRoutesClaudeBackendToAnthropicPath verifies that a claude-backed
// model alias routes through the anthropic backend path. The claude backend is
// served by the anthropic path (the registry rewrites claude to anthropic under
// direct_oauth), so when no anthropic provider is configured the dispatch reports
// upstream_unavailable rather than treating claude as an unsupported backend.
func TestHandleChatRoutesClaudeBackendToAnthropicPath(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

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
	if out.Error.Type != "invalid_request_error" || out.Error.Code != "upstream_unavailable" {
		t.Fatalf("error = %+v body=%s", out.Error, resp.Body.String())
	}
}

func TestHandleChatLogsCursorModelNormalization(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.NativeModelRouting = "codex"
		cfg.Codex.Models = testCodexModels()
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
	if resolved["backend"] != BackendCodex.String() {
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
	srv, buf := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.Models = testCodexModels()
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
	if evt["backend"] != BackendCodex.String() {
		t.Fatalf("backend=%v want %s", evt["backend"], BackendCodex)
	}
}

func TestHandleChatModelResolutionErrorUsesCursorNativeShape(t *testing.T) {
	t.Parallel()
	srv, buf := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		cfg.Codex.Enabled = true
		cfg.Codex.NativeModelRouting = "off"
		cfg.Codex.Models = testCodexModels()
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
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

	if got := srv.Addr(); got != "[::1]:11434" {
		t.Fatalf("Addr()=%q want [::1]:11434", got)
	}
}

func TestAdapterEmitterHonorsConfiguredIncompletePolicy(t *testing.T) {
	t.Parallel()
	logBuffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var handlerCalls int
	options := append(
		adapterEmitterOptions(config.LoggingRequest{IncompletePolicy: "fail_test"}),
		logevent.WithTestingTB(func(report logevent.IncompleteReport) {
			handlerCalls++
			_ = report
		}),
	)
	emitter := logevent.NewEmitter(
		logger,
		logevent.RequiredLegs{logevent.SurfaceAdapterChat: {logevent.LegAdapterIngress, logevent.LegAdapterPayload}},
		options...,
	)
	recorder := emitter.Begin(
		logevent.Identity{RequestID: "policy-probe"},
		logevent.Path{Surface: logevent.SurfaceAdapterChat, RouteFamily: logevent.RouteFamilyChatCompatible},
	)
	recorder.Emit(context.Background(), logevent.Event{Path: logevent.Path{Leg: logevent.LegAdapterIngress, Phase: logevent.PhaseStarted}})
	if recorder.Complete(context.Background()) {
		t.Fatalf("Complete returned true on a missing required leg")
	}

	incomplete := findLogEvent(t, logBuffer, "logging.request.incomplete")
	if incomplete == nil {
		t.Fatalf("expected logging.request.incomplete event")
	}
	if incomplete["incomplete_policy"] != string(logevent.IncompletePolicyFailTest) {
		t.Fatalf("incomplete_policy=%v want %s", incomplete["incomplete_policy"], logevent.IncompletePolicyFailTest)
	}
	if handlerCalls != 1 {
		t.Fatalf("test fail handler called %d times, want 1", handlerCalls)
	}

	if policy := adapterEmitterOptions(config.LoggingRequest{IncompletePolicy: "warn"}); len(policy) != 2 {
		t.Fatalf("warn options length = %d want 2", len(policy))
	}
	if policy := adapterEmitterOptions(config.LoggingRequest{IncompletePolicy: ""}); len(policy) != 2 {
		t.Fatalf("default options length = %d want 2", len(policy))
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

func findPayloadLogEvent(t *testing.T, logBuffer *bytes.Buffer) map[string]any {
	t.Helper()
	return findRequestLegLogEvent(t, logBuffer, logevent.LegAdapterPayload)
}

func findRequestLegLogEvent(t *testing.T, logBuffer *bytes.Buffer, leg logevent.Leg) map[string]any {
	t.Helper()
	for _, event := range findLogEvents(t, logBuffer, "logging.request.leg") {
		if event["surface"] == string(logevent.SurfaceAdapterChat) && event["leg"] == string(leg) {
			return event
		}
	}
	return nil
}

func findLogEvents(t *testing.T, logBuffer *bytes.Buffer, message string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logBuffer.String()), "\n")
	events := make([]map[string]any, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		if evt["msg"] == message {
			events = append(events, evt)
		}
	}
	return events
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

func facetFromEvent(t *testing.T, event map[string]any, key string) map[string]any {
	t.Helper()
	raw, ok := event[key]
	if !ok {
		t.Fatalf("missing facet %q on event %v", key, event)
	}
	facet, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("facet %q has unexpected shape %T", key, raw)
	}
	return facet
}

// TestForceStreamUsageOptInAlwaysSets pins the CLYDE-438 generic adapter
// policy: every streaming completion that flows through the OpenAI route family
// emits the trailing usage chunk regardless of the client's
// `stream_options.include_usage` value.
func TestForceStreamUsageOptInAlwaysSets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input *ChatRequest
	}{
		{
			name:  "nil_stream_options",
			input: &ChatRequest{Model: "clyde-opus-4.7-1m-high-thinking", Stream: true},
		},
		{
			name: "stream_options_opt_in_already",
			input: &ChatRequest{
				Model:         "clyde-codex-5.4-1m-high",
				Stream:        true,
				StreamOptions: &StreamOptions{IncludeUsage: true},
			},
		},
		{
			name: "stream_options_explicit_opt_out",
			input: &ChatRequest{
				Model:         "clyde-opus-4.7-1m-high-thinking",
				Stream:        true,
				StreamOptions: &StreamOptions{IncludeUsage: false},
			},
		},
		{
			name:  "non_streaming_request",
			input: &ChatRequest{Model: "clyde-opus-4.7-1m-high-thinking", Stream: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			forceStreamUsageOptIn(tc.input)
			if tc.input.StreamOptions == nil {
				t.Fatalf("StreamOptions is nil after force; want non-nil with IncludeUsage=true")
			}
			if !tc.input.StreamOptions.IncludeUsage {
				t.Fatalf("IncludeUsage=false after force; want true")
			}
		})
	}
}

// TestForceStreamUsageOptInNilRequestIsNoop ensures the helper is
// defensive against a nil pointer so a future refactor that calls it
// before the body parses cannot panic.
func TestForceStreamUsageOptInNilRequestIsNoop(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("forceStreamUsageOptIn(nil) panicked: %v", r)
		}
	}()
	forceStreamUsageOptIn(nil)
}
