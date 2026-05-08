package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterretry "goodkind.io/clyde/internal/adapter/retry"
)

const overloadedMessageForRetryTest = "Our servers are currently overloaded. Please try again later."

func runWebsocketTransportForTest(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapteropenai.StreamChunk) error,
) (RunResult, error) {
	renderer := adapterrender.NewEventRenderer(cfg.RequestID, cfg.Alias, "codex", nil)
	return RunWebsocketTransportEvents(ctx, cfg, payload, func(event adapterrender.Event) error {
		for _, chunk := range renderer.HandleEvent(event) {
			if err := emit(chunk); err != nil {
				return err
			}
		}
		return nil
	})
}

func mustMarshalTurnMetadataForTest(t *testing.T, metadata TurnMetadata) string {
	t.Helper()
	raw, err := metadata.MarshalCompact()
	if err != nil {
		t.Fatalf("marshal turn metadata: %v", err)
	}
	return raw
}

func TestWebsocketMessageToSyntheticSSEMapsContextWindowError(t *testing.T) {
	_, err := websocketMessageToSyntheticSSE([]byte(`{"type":"error","error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}`))
	if err == nil {
		t.Fatalf("websocketMessageToSyntheticSSE error = nil, want context window error")
	}
	var contextErr *ContextWindowError
	if !errors.As(err, &contextErr) {
		t.Fatalf("websocketMessageToSyntheticSSE error type = %T, want ContextWindowError", err)
	}
}

func TestWebsocketMessageToSyntheticSSEMapsUnsupportedModelError(t *testing.T) {
	_, err := websocketMessageToSyntheticSSE([]byte(`{"type":"error","error":{"message":"unsupported model: gpt-5.5"}}`))
	if err == nil {
		t.Fatalf("websocketMessageToSyntheticSSE error = nil, want unsupported model error")
	}
	var unsupportedErr *UnsupportedModelError
	if !errors.As(err, &unsupportedErr) {
		t.Fatalf("websocketMessageToSyntheticSSE error type = %T, want UnsupportedModelError", err)
	}
}

func TestWebsocketMessageToSyntheticSSEPreservesGenericError(t *testing.T) {
	_, err := websocketMessageToSyntheticSSE([]byte(`{"type":"error","error":{"message":"codex websocket read failed"}}`))
	if err == nil {
		t.Fatalf("websocketMessageToSyntheticSSE error = nil, want generic error")
	}
	var contextErr *ContextWindowError
	if errors.As(err, &contextErr) {
		t.Fatalf("websocketMessageToSyntheticSSE error = %T, want generic error", err)
	}
	var unsupportedErr *UnsupportedModelError
	if errors.As(err, &unsupportedErr) {
		t.Fatalf("websocketMessageToSyntheticSSE error = %T, want generic error", err)
	}
	if err.Error() != "codex websocket read failed" {
		t.Fatalf("generic error = %q", err.Error())
	}
}

func TestResponseCreateRequestFromHTTPUsesResponseCreateShape(t *testing.T) {
	req := HTTPTransportRequest{
		Model:             "gpt-5.4",
		Instructions:      "base instructions",
		Input:             []map[string]any{{"type": "message", "role": "user"}},
		Tools:             []any{map[string]any{"type": "function", "name": "read_file"}},
		ToolChoice:        "auto",
		ParallelToolCalls: true,
		Reasoning:         &Reasoning{Effort: "medium"},
		Include:           []string{"reasoning.encrypted_content"},
		ServiceTier:       "priority",
		PromptCache:       "cursor:conv-123",
		ClientMetadata:    map[string]string{"x-codex-installation-id": "acct-123"},
		Store:             false,
		Stream:            true,
	}

	ws := ResponseCreateRequestFromHTTP(req)
	if ws.Type != "response.create" {
		t.Fatalf("type=%q want response.create", ws.Type)
	}
	if ws.ServiceTier != "priority" {
		t.Fatalf("service_tier=%q want priority", ws.ServiceTier)
	}
	if ws.Store {
		t.Fatalf("store=%v want false", ws.Store)
	}
	if !ws.Stream {
		t.Fatalf("stream=%v want true", ws.Stream)
	}

	encoded, err := MarshalResponseCreateWsRequest(ws)
	if err != nil {
		t.Fatalf("marshal websocket request: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got, _ := payload["type"].(string); got != "response.create" {
		t.Fatalf("serialized type=%q want response.create", got)
	}
	if got, _ := payload["service_tier"].(string); got != "priority" {
		t.Fatalf("serialized service_tier=%q want priority", got)
	}
	if got, ok := payload["store"].(bool); !ok || got {
		t.Fatalf("serialized store=%v want false", payload["store"])
	}
	if got, ok := payload["stream"].(bool); !ok || !got {
		t.Fatalf("serialized stream=%v want true", payload["stream"])
	}
}

func TestWithWarmupGenerateFalseSetsGenerateFlag(t *testing.T) {
	ws := WithWarmupGenerateFalse(ResponseCreateWsRequest{Type: "response.create"})
	ws.Tools = []any{}
	if ws.Generate == nil || *ws.Generate {
		t.Fatalf("generate=%v want false", ws.Generate)
	}
	encoded, err := MarshalResponseCreateWsRequest(ws)
	if err != nil {
		t.Fatalf("marshal websocket request: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got, ok := payload["generate"].(bool); !ok || got {
		t.Fatalf("serialized generate=%v want false", payload["generate"])
	}
	if tools, ok := payload["tools"].([]any); !ok || len(tools) != 0 {
		t.Fatalf("serialized tools=%v want empty array", payload["tools"])
	}
}

func TestWithPreviousResponseIDOverridesInputIncrementally(t *testing.T) {
	base := ResponseCreateWsRequest{
		Type:  "response.create",
		Input: []map[string]any{{"type": "message", "role": "user", "content": "full"}},
	}
	incremental := []map[string]any{{"type": "message", "role": "user", "content": "delta"}}
	ws := WithPreviousResponseID(base, "resp-123", incremental)
	if ws.PreviousResponseID != "resp-123" {
		t.Fatalf("previous_response_id=%q want resp-123", ws.PreviousResponseID)
	}
	if len(ws.Input) != 1 || ws.Input[0]["content"] != "delta" {
		t.Fatalf("input=%v want incremental delta", ws.Input)
	}
	encoded, err := MarshalResponseCreateWsRequest(ws)
	if err != nil {
		t.Fatalf("marshal websocket request: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got, _ := payload["previous_response_id"].(string); got != "resp-123" {
		t.Fatalf("serialized previous_response_id=%q want resp-123", got)
	}

	ws = WithPreviousResponseID(base, "resp-123", []map[string]any{})
	encoded, err = MarshalResponseCreateWsRequest(ws)
	if err != nil {
		t.Fatalf("marshal websocket request with empty input: %v", err)
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal empty-input payload: %v", err)
	}
	if input, ok := payload["input"].([]any); !ok || len(input) != 0 {
		t.Fatalf("serialized input=%v want empty array", payload["input"])
	}
}

func TestRunWebsocketTransportReturnsFallbackOnUpgradeRequired(t *testing.T) {
	t.Parallel()

	// We do not need a full websocket server here; we only need the
	// handshake status. Point the websocket dial at an HTTP server that
	// returns 426 Upgrade Required so the transport surfaces the same
	// fallback signal codex-rs uses.
	server := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
	})
	ts := httptest.NewServer(server)
	defer ts.Close()

	_, err := runWebsocketTransportForTest(context.Background(), WebsocketTransportConfig{
		URL:       "ws" + strings.TrimPrefix(ts.URL, "http"),
		Token:     "test-token",
		RequestID: "req-1",
		Alias:     "gpt-5.4",
	}, ResponseCreateWsRequest{Type: "response.create"}, func(adapteropenai.StreamChunk) error {
		return nil
	})
	if !errors.Is(err, ErrWebsocketFallbackToHTTP) {
		t.Fatalf("err=%v want ErrWebsocketFallbackToHTTP", err)
	}
}

func TestRunWebsocketTransportParsesTextAndCompletion(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	turnState := NewTurnState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-client-request-id"); got != "cursor:conv-123" {
			t.Fatalf("x-client-request-id=%q want cursor:conv-123", got)
		}
		if got := r.Header.Get("session_id"); got != "cursor:conv-123" {
			t.Fatalf("session_id=%q want cursor:conv-123", got)
		}
		if got := r.Header.Get(CodexWindowIDHeader); got != "cursor:conv-123:0" {
			t.Fatalf("%s=%q want cursor:conv-123:0", CodexWindowIDHeader, got)
		}
		// x-codex-installation-id now comes from LoadInstallationID
		// (~/.codex/installation_id or persisted clyde uuid) rather
		// than the auth account_id. Only assert non-empty here.
		if got := r.Header.Get(CodexInstallationIDHeader); got == "" {
			t.Fatalf("%s should be non-empty", CodexInstallationIDHeader)
		}
		if got := r.Header.Get(CodexOriginatorHeader); got != CodexOriginatorValue {
			t.Fatalf("%s=%q want %s", CodexOriginatorHeader, got, CodexOriginatorValue)
		}
		if got := r.Header.Get(CodexTurnMetadataHeader); got == "" {
			t.Fatalf("%s should be non-empty", CodexTurnMetadataHeader)
		}
		var turnMetadata TurnMetadata
		if err := json.Unmarshal([]byte(r.Header.Get(CodexTurnMetadataHeader)), &turnMetadata); err != nil {
			t.Fatalf("parse %s: %v", CodexTurnMetadataHeader, err)
		}
		if _, ok := turnMetadata.Workspaces["/workspace"]; !ok {
			t.Fatalf("%s missing workspace metadata: %#v", CodexTurnMetadataHeader, turnMetadata.Workspaces)
		}
		responseHeader := http.Header{}
		responseHeader.Set(CodexTurnStateHeader, "turn-123")
		conn, err := upgrader.Upgrade(w, r, responseHeader)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		_, requestBody, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var request map[string]any
		if err := json.Unmarshal(requestBody, &request); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if got, _ := request["type"].(string); got != "response.create" {
			t.Fatalf("request type=%q want response.create", got)
		}

		events := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-1"}},
			{"type": "response.output_text.delta", "delta": "hello "},
			{"type": "response.output_text.delta", "delta": "world"},
			{"type": "response.completed", "response": map[string]any{"id": "resp-1", "usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
		}
		for _, event := range events {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	var chunks []adapteropenai.StreamChunk
	result, err := runWebsocketTransportForTest(context.Background(), WebsocketTransportConfig{
		URL:            wsURL,
		Token:          "test-token",
		AccountID:      "acct-123",
		RequestID:      "req-ws",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-123",
		TurnState:      turnState,
		TurnMetadata: mustMarshalTurnMetadataForTest(t, NewTurnMetadata("cursor:conv-123", "").
			WithWorkspace("/workspace", TurnMetadataWorkspace{HasChanges: true})),
	}, ResponseCreateWsRequest{Type: "response.create"}, func(ch adapteropenai.StreamChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("RunWebsocketTransport: %v", err)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("finish_reason=%q want stop", result.FinishReason)
	}
	if result.ResponseID != "resp-1" {
		t.Fatalf("response_id=%q want resp-1", result.ResponseID)
	}
	if got := turnState.Value(); got != "turn-123" {
		t.Fatalf("turn_state=%q want turn-123", got)
	}
	if result.Usage.PromptTokens != 10 || result.Usage.CompletionTokens != 4 || result.Usage.TotalTokens != 14 {
		t.Fatalf("usage=%+v", result.Usage)
	}
	var content strings.Builder
	for _, ch := range chunks {
		if len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if got := content.String(); got != "hello world" {
		t.Fatalf("content=%q want hello world", got)
	}
}

func TestRunWebsocketTransportEmitsDelayedDeltasBeforeCompletion(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	firstDeltaEmitted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read request: %v", err)
		}
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-streaming"}},
			{"type": "response.output_text.delta", "delta": "first "},
		} {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}

		select {
		case <-firstDeltaEmitted:
		case <-time.After(time.Second):
			t.Fatal("first delta was not emitted before completion")
		}

		for _, event := range []map[string]any{
			{"type": "response.output_text.delta", "delta": "second"},
			{"type": "response.completed", "response": map[string]any{"id": "resp-streaming", "usage": map[string]any{"input_tokens": 1, "output_tokens": 2, "total_tokens": 3, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
		} {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	var content strings.Builder
	result, err := RunWebsocketTransportEvents(context.Background(), WebsocketTransportConfig{
		URL:       "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:     "test-token",
		RequestID: "req-delayed-stream",
		Alias:     "gpt-5.4",
	}, ResponseCreateWsRequest{Type: "response.create"}, func(event adapterrender.Event) error {
		if td, ok := event.(adapterrender.TextDelta); ok {
			content.WriteString(td.Text)
			if td.Text == "first " {
				close(firstDeltaEmitted)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunWebsocketTransportEvents: %v", err)
	}
	if result.ResponseID != "resp-streaming" {
		t.Fatalf("response_id=%q want resp-streaming", result.ResponseID)
	}
	if got := content.String(); got != "first second" {
		t.Fatalf("content=%q want first second", got)
	}
}

func TestRunWebsocketTransportReturnsTopLevelErrorFrame(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read request: %v", err)
		}
		payload, err := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"message": "Unsupported parameter: prompt_cache_retention",
				"type":    "invalid_request_error",
			},
		})
		if err != nil {
			t.Fatalf("marshal error frame: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			t.Fatalf("write error frame: %v", err)
		}
	}))
	defer server.Close()

	_, err := runWebsocketTransportForTest(context.Background(), WebsocketTransportConfig{
		URL:       "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:     "test-token",
		RequestID: "req-ws",
		Alias:     "gpt-5.4",
	}, ResponseCreateWsRequest{Type: "response.create"}, func(adapteropenai.StreamChunk) error {
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "Unsupported parameter: prompt_cache_retention") {
		t.Fatalf("err=%v want upstream websocket error", err)
	}
}

func TestRunWebsocketTransportRetriesOverloadedThenSucceeds(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read request: %v", err)
		}
		if attempt == 1 {
			writeWebsocketEventForTest(t, conn, map[string]any{
				"type":  "response.failed",
				"error": map[string]any{"message": overloadedMessageForRetryTest},
			})
			return
		}
		writeWebsocketEventForTest(t, conn, map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-retry"}})
		writeWebsocketEventForTest(t, conn, map[string]any{"type": "response.output_text.delta", "delta": "ok"})
		writeWebsocketEventForTest(t, conn, map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp-retry", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}})
	}))
	defer server.Close()

	var text strings.Builder
	result, err := runWebsocketTransportEventsWithRetry(context.Background(), WebsocketTransportConfig{
		URL:           "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:         "test-token",
		RequestID:     "req-retry-success",
		Alias:         "gpt-5.4",
		RetryPolicies: codexOverloadedRetryPolicyForTest(),
	}, ResponseCreateWsRequest{Type: "response.create"}, func(event adapterrender.Event) error {
		if td, ok := event.(adapterrender.TextDelta); ok {
			text.WriteString(td.Text)
		}
		return nil
	}, func(context.Context, time.Duration) error { return nil })
	if err != nil {
		t.Fatalf("RunWebsocketTransportEvents: %v", err)
	}
	if result.ResponseID != "resp-retry" {
		t.Fatalf("response_id=%q want resp-retry", result.ResponseID)
	}
	if text.String() != "ok" {
		t.Fatalf("text=%q want ok", text.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d want 2", attempts.Load())
	}
}

func TestRunWebsocketTransportExhaustsOverloadedRetries(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read request: %v", err)
		}
		writeWebsocketEventForTest(t, conn, map[string]any{
			"type":  "response.failed",
			"error": map[string]any{"message": overloadedMessageForRetryTest},
		})
	}))
	defer server.Close()

	_, err := runWebsocketTransportEventsWithRetry(context.Background(), WebsocketTransportConfig{
		URL:           "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:         "test-token",
		RequestID:     "req-retry-exhausted",
		Alias:         "gpt-5.4",
		RetryPolicies: codexOverloadedRetryPolicyForTest(),
	}, ResponseCreateWsRequest{Type: "response.create"}, func(adapterrender.Event) error {
		return nil
	}, func(context.Context, time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), overloadedMessageForRetryTest) {
		t.Fatalf("err=%v want overloaded error", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts=%d want 3", attempts.Load())
	}
}

func TestRunWebsocketTransportDoesNotRetryNonRetryableFailure(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read request: %v", err)
		}
		writeWebsocketEventForTest(t, conn, map[string]any{
			"type":  "response.failed",
			"error": map[string]any{"message": "Your input exceeds the context window of this model. Please adjust your input and try again."},
		})
	}))
	defer server.Close()

	_, err := runWebsocketTransportEventsWithRetry(context.Background(), WebsocketTransportConfig{
		URL:           "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:         "test-token",
		RequestID:     "req-retry-nonretryable",
		Alias:         "gpt-5.4",
		RetryPolicies: codexOverloadedRetryPolicyForTest(),
	}, ResponseCreateWsRequest{Type: "response.create"}, func(adapterrender.Event) error {
		return nil
	}, func(context.Context, time.Duration) error { return nil })
	if err == nil {
		t.Fatalf("err=nil want context window error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts=%d want 1", attempts.Load())
	}
}

func TestRunWebsocketTransportDoesNotRetryAfterClientVisibleOutput(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read request: %v", err)
		}
		writeWebsocketEventForTest(t, conn, map[string]any{"type": "response.output_text.delta", "delta": "partial"})
		writeWebsocketEventForTest(t, conn, map[string]any{
			"type":  "response.failed",
			"error": map[string]any{"message": overloadedMessageForRetryTest},
		})
	}))
	defer server.Close()

	var text strings.Builder
	_, err := runWebsocketTransportEventsWithRetry(context.Background(), WebsocketTransportConfig{
		URL:           "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:         "test-token",
		RequestID:     "req-retry-started",
		Alias:         "gpt-5.4",
		RetryPolicies: codexOverloadedRetryPolicyForTest(),
	}, ResponseCreateWsRequest{Type: "response.create"}, func(event adapterrender.Event) error {
		if td, ok := event.(adapterrender.TextDelta); ok {
			text.WriteString(td.Text)
		}
		return nil
	}, func(context.Context, time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), overloadedMessageForRetryTest) {
		t.Fatalf("err=%v want overloaded error", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts=%d want 1", attempts.Load())
	}
	if text.String() != "partial" {
		t.Fatalf("text=%q want partial", text.String())
	}
}

func codexOverloadedRetryPolicyForTest() []adapterretry.Policy {
	return []adapterretry.Policy{{
		Name:                     "codex.responses.overloaded",
		Enabled:                  true,
		MaxAttempts:              3,
		InitialDelay:             0,
		MaxDelay:                 0,
		Multiplier:               1,
		JitterFraction:           0,
		RetryWhenResponseStarted: false,
		Match: adapterretry.Matchers{
			Backends:          []string{"codex"},
			Operations:        []string{"codex.responses.websocket.generate"},
			ErrorClasses:      []string{"response_failed", "websocket_error", "scanner_error"},
			MessageSubstrings: []string{overloadedMessageForRetryTest},
		},
	}}
}

func writeWebsocketEventForTest(t *testing.T, conn *websocket.Conn, event map[string]any) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("write event: %v", err)
	}
}

func TestRunWebsocketTransportCacheReusesConnectionAndChainsResponseIDs(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var handshakes atomic.Int32
	requestsCh := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()
		var requests []map[string]any
		respIDs := []string{"warmup-1", "resp-1", "resp-2", "resp-3"}
		// Server expects 4 frames: warmup + 3 real turns. Each
		// completion carries the matching response_id so the client
		// can chain previous_response_id.
		for idx := range 4 {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal request %d: %v", idx, err)
			}
			requests = append(requests, req)
			events := []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": respIDs[idx]}},
				{"type": "response.completed", "response": map[string]any{"id": respIDs[idx], "usage": map[string]any{"input_tokens": 1, "output_tokens": 0, "total_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
			}
			for _, event := range events {
				payload, err := json.Marshal(event)
				if err != nil {
					t.Fatalf("marshal event: %v", err)
				}
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return
				}
			}
		}
		requestsCh <- requests
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cache := NewWebsocketSessionCache(nil, time.Minute, nil)
	cfg := WebsocketTransportConfig{
		URL:            wsURL,
		Token:          "test-token",
		RequestID:      "req-cache",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-cache",
		SessionCache:   cache,
		TurnState:      NewTurnState(),
	}

	turn := func(items []map[string]any) {
		_, err := runWebsocketTransportForTest(context.Background(), cfg, ResponseCreateWsRequest{
			Type:  "response.create",
			Model: "gpt-5.4",
			Input: items,
		}, func(adapteropenai.StreamChunk) error { return nil })
		if err != nil {
			t.Fatalf("turn: %v", err)
		}
	}

	turn1 := []map[string]any{{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "first"}}}}
	turn2 := append([]map[string]any{}, turn1...)
	turn2 = append(turn2, map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "ack-1"}}})
	turn2 = append(turn2, map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "second"}}})
	turn3 := append([]map[string]any{}, turn2...)
	turn3 = append(turn3, map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "ack-2"}}})
	turn3 = append(turn3, map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "third"}}})

	turn(turn1)
	turn(turn2)
	turn(turn3)

	if got := handshakes.Load(); got != 1 {
		t.Fatalf("expected 1 ws handshake across 3 turns, got %d", got)
	}
	requests := <-requestsCh
	if len(requests) != 4 {
		t.Fatalf("expected 4 frames (warmup + 3 turns), got %d", len(requests))
	}

	// Frame 0: warmup. generate=false, no prev, empty input.
	if got, _ := requests[0]["generate"].(bool); got {
		t.Errorf("warmup generate=%v want false", requests[0]["generate"])
	}
	if got, ok := requests[0]["previous_response_id"].(string); ok && got != "" {
		t.Errorf("warmup previous_response_id=%q want empty", got)
	}

	// Frame 1: turn 1 real. prev=warmup-1, full input (1 item).
	if got, _ := requests[1]["previous_response_id"].(string); got != "warmup-1" {
		t.Errorf("turn1 prev=%q want warmup-1", got)
	}
	if input, _ := requests[1]["input"].([]any); len(input) != 1 {
		t.Errorf("turn1 input count=%d want 1 (full)", len(input))
	}

	// Frame 2: turn 2 real. prev=resp-1, delta input (2 new items).
	if got, _ := requests[2]["previous_response_id"].(string); got != "resp-1" {
		t.Errorf("turn2 prev=%q want resp-1", got)
	}
	if input, _ := requests[2]["input"].([]any); len(input) != 2 {
		t.Errorf("turn2 input count=%d want 2 (delta)", len(input))
	}

	// Frame 3: turn 3 real. prev=resp-2, delta input (2 new items).
	if got, _ := requests[3]["previous_response_id"].(string); got != "resp-2" {
		t.Errorf("turn3 prev=%q want resp-2", got)
	}
	if input, _ := requests[3]["input"].([]any); len(input) != 2 {
		t.Errorf("turn3 input count=%d want 2 (delta)", len(input))
	}
}

func TestRunWebsocketTransportInvalidatesTakenSessionOnDeltaMismatch(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var handshakes atomic.Int32
	closedOldConn := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connNumber := handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		frameCount := 2
		if connNumber == 2 {
			frameCount = 2
		}
		for idx := range frameCount {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
			responseID := "resp-1"
			if connNumber == 1 && idx == 0 {
				responseID = "warm-1"
			}
			if connNumber == 2 && idx == 0 {
				responseID = "warm-2"
			}
			if connNumber == 2 && idx == 1 {
				responseID = "resp-2"
			}
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": responseID}},
				{"type": "response.completed", "response": map[string]any{"id": responseID, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0, "total_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
			} {
				payload, err := json.Marshal(event)
				if err != nil {
					t.Fatalf("marshal event: %v", err)
				}
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return
				}
			}
		}
		if connNumber == 1 {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _, err := conn.ReadMessage()
			if err != nil {
				closedOldConn <- struct{}{}
			}
		}
	}))
	defer server.Close()

	cache := NewWebsocketSessionCache(nil, time.Minute, nil)
	cfg := WebsocketTransportConfig{
		URL:            "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:          "test-token",
		RequestID:      "req-cache-mismatch",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-cache",
		SessionCache:   cache,
		TurnState:      NewTurnState(),
	}

	first := []map[string]any{{"type": "message", "role": "user", "content": "first"}}
	_, err := runWebsocketTransportForTest(context.Background(), cfg, ResponseCreateWsRequest{
		Type:  "response.create",
		Model: "gpt-5.4",
		Input: first,
	}, func(adapteropenai.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}

	mismatched := []map[string]any{{"type": "message", "role": "user", "content": "different root"}}
	_, err = runWebsocketTransportForTest(context.Background(), cfg, ResponseCreateWsRequest{
		Type:  "response.create",
		Model: "gpt-5.4",
		Input: mismatched,
	}, func(adapteropenai.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("mismatched turn: %v", err)
	}

	if handshakes.Load() != 2 {
		t.Fatalf("handshakes=%d want 2", handshakes.Load())
	}
	select {
	case <-closedOldConn:
	case <-time.After(2 * time.Second):
		t.Fatal("old websocket was not closed after delta mismatch")
	}
}

func TestRunWebsocketTransportInvalidatesTakenSessionOnModelMismatch(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var handshakes atomic.Int32
	requestsCh := make(chan []map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connNumber := handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		var requests []map[string]any
		for idx := range 2 {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]any
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("unmarshal request: %v", err)
			}
			requests = append(requests, req)
			responseID := "resp-a"
			if idx == 0 {
				responseID = "warm-a"
			}
			if connNumber == 2 {
				responseID = "resp-b"
				if idx == 0 {
					responseID = "warm-b"
				}
			}
			for _, event := range []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": responseID}},
				{"type": "response.completed", "response": map[string]any{"id": responseID, "usage": map[string]any{"input_tokens": 1, "output_tokens": 0, "total_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
			} {
				payload, err := json.Marshal(event)
				if err != nil {
					t.Fatalf("marshal event: %v", err)
				}
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					return
				}
			}
		}
		requestsCh <- requests
	}))
	defer server.Close()

	cache := NewWebsocketSessionCache(nil, time.Minute, nil)
	cfg := WebsocketTransportConfig{
		URL:            "ws" + strings.TrimPrefix(server.URL, "http"),
		Token:          "test-token",
		RequestID:      "req-cache-model",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-cache",
		SessionCache:   cache,
		TurnState:      NewTurnState(),
	}
	input := []map[string]any{{"type": "message", "role": "user", "content": "first"}}

	_, err := runWebsocketTransportForTest(context.Background(), cfg, ResponseCreateWsRequest{
		Type:           "response.create",
		Model:          "gpt-5.4",
		PromptCacheKey: "cursor:conv-cache",
		Input:          input,
	}, func(adapteropenai.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}

	_, err = runWebsocketTransportForTest(context.Background(), cfg, ResponseCreateWsRequest{
		Type:           "response.create",
		Model:          "gpt-5.5",
		PromptCacheKey: "cursor:conv-cache",
		Input:          append(input, map[string]any{"type": "message", "role": "user", "content": "second"}),
	}, func(adapteropenai.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}

	if got := handshakes.Load(); got != 2 {
		t.Fatalf("handshakes=%d want 2", got)
	}
	firstRequests := <-requestsCh
	secondRequests := <-requestsCh
	if got, _ := firstRequests[1]["previous_response_id"].(string); got != "warm-a" {
		t.Fatalf("first real frame prev=%q want warm-a", got)
	}
	if got, _ := secondRequests[1]["previous_response_id"].(string); got != "warm-b" {
		t.Fatalf("second real frame prev=%q want warm-b", got)
	}
	if input, _ := secondRequests[1]["input"].([]any); len(input) != 2 {
		t.Fatalf("second real frame input len=%d want full input after model mismatch", len(input))
	}
}

func TestRunWebsocketTransportPrewarmsAndReusesConnection(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var handshakes atomic.Int32
	requestsCh := make(chan []map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		var requests []map[string]any
		for idx := range 2 {
			_, requestBody, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read request %d: %v", idx, err)
			}
			var request map[string]any
			if err := json.Unmarshal(requestBody, &request); err != nil {
				t.Fatalf("unmarshal request %d: %v", idx, err)
			}
			requests = append(requests, request)
			events := []map[string]any{
				{"type": "response.created", "response": map[string]any{"id": "warm-1"}},
				{"type": "response.completed", "response": map[string]any{"id": "warm-1", "usage": map[string]any{"input_tokens": 1, "output_tokens": 0, "total_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
			}
			if idx == 1 {
				events = []map[string]any{
					{"type": "response.created", "response": map[string]any{"id": "resp-1"}},
					{"type": "response.output_text.delta", "delta": "done"},
					{"type": "response.completed", "response": map[string]any{"id": "resp-1", "usage": map[string]any{"input_tokens": 10, "output_tokens": 1, "total_tokens": 11, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
				}
			}
			for _, event := range events {
				payload, err := json.Marshal(event)
				if err != nil {
					t.Fatalf("marshal event: %v", err)
				}
				if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
					t.Fatalf("write event: %v", err)
				}
			}
		}
		requestsCh <- requests
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	var chunks []adapteropenai.StreamChunk
	result, err := runWebsocketTransportForTest(context.Background(), WebsocketTransportConfig{
		URL:            wsURL,
		Token:          "test-token",
		RequestID:      "req-ws",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-123",
		TurnState:      NewTurnState(),
		Prewarm:        true,
	}, ResponseCreateWsRequest{
		Type:  "response.create",
		Model: "gpt-5.4",
		Input: []map[string]any{{"type": "message", "role": "user", "content": "hello"}},
		Tools: []any{map[string]any{"type": "function", "name": "read_file"}},
	}, func(ch adapteropenai.StreamChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("RunWebsocketTransport: %v", err)
	}
	if result.ResponseID != "resp-1" {
		t.Fatalf("response_id=%q want resp-1", result.ResponseID)
	}
	if handshakes.Load() != 1 {
		t.Fatalf("handshakes=%d want 1", handshakes.Load())
	}
	requests := <-requestsCh
	if len(requests) != 2 {
		t.Fatalf("requests len=%d want 2", len(requests))
	}
	if got, ok := requests[0]["generate"].(bool); !ok || got {
		t.Fatalf("warmup generate=%v want false", requests[0]["generate"])
	}
	if tools, ok := requests[0]["tools"].([]any); !ok || len(tools) != 0 {
		t.Fatalf("warmup tools=%v want empty array", requests[0]["tools"])
	}
	if got, _ := requests[1]["previous_response_id"].(string); got != "warm-1" {
		t.Fatalf("follow-up previous_response_id=%q want warm-1", got)
	}
	if input, ok := requests[1]["input"].([]any); !ok || len(input) != 0 {
		t.Fatalf("follow-up input=%v want empty array", requests[1]["input"])
	}
	var content strings.Builder
	for _, ch := range chunks {
		if len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if got := content.String(); got != "done" {
		t.Fatalf("content=%q want done", got)
	}
}

func TestRunWebsocketTransportReconnectsAfterPrewarmFailure(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var handshakes atomic.Int32
	requestsCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connNumber := handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		_, requestBody, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var request map[string]any
		if err := json.Unmarshal(requestBody, &request); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if connNumber == 1 {
			payload, err := json.Marshal(map[string]any{
				"type":  "response.failed",
				"error": map[string]any{"message": "warmup failed"},
			})
			if err != nil {
				t.Fatalf("marshal failure: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write failure: %v", err)
			}
			return
		}
		requestsCh <- request
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-1"}},
			{"type": "response.output_text.delta", "delta": "recovered"},
			{"type": "response.completed", "response": map[string]any{"id": "resp-1", "usage": map[string]any{"input_tokens": 10, "output_tokens": 1, "total_tokens": 11, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
		} {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	var chunks []adapteropenai.StreamChunk
	result, err := runWebsocketTransportForTest(context.Background(), WebsocketTransportConfig{
		URL:            wsURL,
		Token:          "test-token",
		RequestID:      "req-ws",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-123",
		TurnState:      NewTurnState(),
		Prewarm:        true,
	}, ResponseCreateWsRequest{
		Type:  "response.create",
		Model: "gpt-5.4",
		Input: []map[string]any{{"type": "message", "role": "user", "content": "hello"}},
	}, func(ch adapteropenai.StreamChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("RunWebsocketTransport: %v", err)
	}
	if result.ResponseID != "resp-1" {
		t.Fatalf("response_id=%q want resp-1", result.ResponseID)
	}
	if handshakes.Load() != 2 {
		t.Fatalf("handshakes=%d want 2", handshakes.Load())
	}
	request := <-requestsCh
	if _, ok := request["previous_response_id"]; ok {
		t.Fatalf("generated request after failed prewarm should not use previous_response_id: %v", request)
	}
	var content strings.Builder
	for _, ch := range chunks {
		if len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if got := content.String(); got != "recovered" {
		t.Fatalf("content=%q want recovered", got)
	}
}

func TestRunWebsocketTransportCacheFallsBackUncachedAfterWarmupFailure(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var handshakes atomic.Int32
	requestsCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connNumber := handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		_, requestBody, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var request map[string]any
		if err := json.Unmarshal(requestBody, &request); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if connNumber == 1 {
			payload, err := json.Marshal(map[string]any{
				"type":  "response.failed",
				"error": map[string]any{"message": "Instructions are required"},
			})
			if err != nil {
				t.Fatalf("marshal failure: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write failure: %v", err)
			}
			return
		}
		requestsCh <- request
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-uncached"}},
			{"type": "response.output_text.delta", "delta": "recovered"},
			{"type": "response.completed", "response": map[string]any{"id": "resp-uncached", "usage": map[string]any{"input_tokens": 10, "output_tokens": 1, "total_tokens": 11, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
		} {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cache := NewWebsocketSessionCache(nil, time.Minute, nil)
	var chunks []adapteropenai.StreamChunk
	result, err := runWebsocketTransportForTest(context.Background(), WebsocketTransportConfig{
		URL:            wsURL,
		Token:          "test-token",
		RequestID:      "req-cache-warmup-fallback",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-cache-warmup-fallback",
		SessionCache:   cache,
		TurnState:      NewTurnState(),
	}, ResponseCreateWsRequest{
		Type:  "response.create",
		Model: "gpt-5.4",
		Input: []map[string]any{{"type": "message", "role": "user", "content": "hello"}},
	}, func(ch adapteropenai.StreamChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("RunWebsocketTransport: %v", err)
	}
	if result.ResponseID != "resp-uncached" {
		t.Fatalf("response_id=%q want resp-uncached", result.ResponseID)
	}
	if handshakes.Load() != 2 {
		t.Fatalf("handshakes=%d want 2", handshakes.Load())
	}
	request := <-requestsCh
	if _, ok := request["previous_response_id"]; ok {
		t.Fatalf("fallback request should not use previous_response_id: %v", request)
	}
	var content strings.Builder
	for _, ch := range chunks {
		if len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if got := content.String(); got != "recovered" {
		t.Fatalf("content=%q want recovered", got)
	}
	if _, ok := cache.Take(context.Background(), "cursor:conv-cache-warmup-fallback"); ok {
		t.Fatalf("fallback request should not populate cache")
	}
}

func TestRunWebsocketTransportTimesOutHungPrewarmAndRunsGeneratedRequest(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{}
	var handshakes atomic.Int32
	requestsCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connNumber := handshakes.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer func() { _ = conn.Close() }()

		_, requestBody, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var request map[string]any
		if err := json.Unmarshal(requestBody, &request); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if connNumber == 1 {
			if got, ok := request["generate"].(bool); !ok || got {
				t.Fatalf("warmup generate=%v want false", request["generate"])
			}
			time.Sleep(100 * time.Millisecond)
			return
		}
		requestsCh <- request
		for _, event := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp-1"}},
			{"type": "response.output_text.delta", "delta": "after-timeout"},
			{"type": "response.completed", "response": map[string]any{"id": "resp-1", "usage": map[string]any{"input_tokens": 10, "output_tokens": 1, "total_tokens": 11, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
		} {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal event: %v", err)
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("write event: %v", err)
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	var chunks []adapteropenai.StreamChunk
	result, err := runWebsocketTransportForTest(context.Background(), WebsocketTransportConfig{
		URL:            wsURL,
		Token:          "test-token",
		RequestID:      "req-ws",
		Alias:          "gpt-5.4",
		ConversationID: "cursor:conv-123",
		TurnState:      NewTurnState(),
		Prewarm:        true,
		PrewarmTimeout: 20 * time.Millisecond,
	}, ResponseCreateWsRequest{
		Type:  "response.create",
		Model: "gpt-5.4",
		Input: []map[string]any{{"type": "message", "role": "user", "content": "hello"}},
	}, func(ch adapteropenai.StreamChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("RunWebsocketTransport: %v", err)
	}
	if result.ResponseID != "resp-1" {
		t.Fatalf("response_id=%q want resp-1", result.ResponseID)
	}
	if handshakes.Load() != 2 {
		t.Fatalf("handshakes=%d want 2", handshakes.Load())
	}
	request := <-requestsCh
	if _, ok := request["previous_response_id"]; ok {
		t.Fatalf("generated request after timed-out prewarm should not use previous_response_id: %v", request)
	}
	var content strings.Builder
	for _, ch := range chunks {
		if len(ch.Choices) > 0 {
			content.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	if got := content.String(); got != "after-timeout" {
		t.Fatalf("content=%q want after-timeout", got)
	}
}

func TestCodexTransportParityMatrixSerialization(t *testing.T) {
	t.Parallel()

	maxCompletion := 3072
	httpReq := HTTPTransportRequest{
		Model:                "gpt-5.4",
		Instructions:         "base instructions",
		Input:                []map[string]any{{"type": "message", "role": "user", "content": "hello"}},
		Tools:                []any{map[string]any{"type": "function", "name": "read_file"}},
		ToolChoice:           "auto",
		ParallelToolCalls:    true,
		Reasoning:            &Reasoning{Effort: "medium"},
		Store:                false,
		Stream:               true,
		Include:              []string{"reasoning.encrypted_content"},
		ServiceTier:          "priority",
		PromptCache:          "cursor:conv-123",
		PromptCacheRetention: "24h",
		Text:                 json.RawMessage(`{"verbosity":"high"}`),
		Truncation:           "auto",
		MaxCompletion:        &maxCompletion,
	}

	httpEncoded, err := json.Marshal(httpReq)
	if err != nil {
		t.Fatalf("marshal http request: %v", err)
	}
	var httpPayload map[string]any
	if err := json.Unmarshal(httpEncoded, &httpPayload); err != nil {
		t.Fatalf("unmarshal http payload: %v", err)
	}
	if got, _ := httpPayload["model"].(string); got != "gpt-5.4" {
		t.Fatalf("http model=%q want gpt-5.4", got)
	}
	if got, _ := httpPayload["service_tier"].(string); got != "priority" {
		t.Fatalf("http service_tier=%q want priority", got)
	}
	if got, _ := httpPayload["max_completion_tokens"].(float64); int(got) != maxCompletion {
		t.Fatalf("http max_completion_tokens=%v want %d", httpPayload["max_completion_tokens"], maxCompletion)
	}
	if got, _ := httpPayload["prompt_cache_retention"].(string); got != "24h" {
		t.Fatalf("http prompt_cache_retention=%q want 24h", got)
	}
	if got, _ := httpPayload["truncation"].(string); got != "auto" {
		t.Fatalf("http truncation=%q want auto", got)
	}
	text, _ := httpPayload["text"].(map[string]any)
	if text["verbosity"] != "high" {
		t.Fatalf("http text=%v want verbosity high", httpPayload["text"])
	}

	wsReq := ResponseCreateRequestFromHTTP(httpReq)
	wsReq = WithWarmupGenerateFalse(wsReq)
	wsReq = WithPreviousResponseID(wsReq, "resp-123", []map[string]any{{"type": "message", "role": "user", "content": "delta"}})
	wsEncoded, err := MarshalResponseCreateWsRequest(wsReq)
	if err != nil {
		t.Fatalf("marshal ws request: %v", err)
	}
	var wsPayload map[string]any
	if err := json.Unmarshal(wsEncoded, &wsPayload); err != nil {
		t.Fatalf("unmarshal ws payload: %v", err)
	}
	if got, _ := wsPayload["type"].(string); got != "response.create" {
		t.Fatalf("ws type=%q want response.create", got)
	}
	if got, _ := wsPayload["service_tier"].(string); got != "priority" {
		t.Fatalf("ws service_tier=%q want priority", got)
	}
	for _, key := range []string{"max_completion_tokens", "prompt_cache_retention", "truncation"} {
		if _, ok := wsPayload[key]; ok {
			t.Fatalf("ws payload included HTTP-only key %q: %v", key, wsPayload)
		}
	}
	if got, ok := wsPayload["store"].(bool); !ok || got {
		t.Fatalf("ws store=%v want false", wsPayload["store"])
	}
	if got, ok := wsPayload["stream"].(bool); !ok || !got {
		t.Fatalf("ws stream=%v want true", wsPayload["stream"])
	}
	text, _ = wsPayload["text"].(map[string]any)
	if text["verbosity"] != "high" {
		t.Fatalf("ws text=%v want verbosity high", wsPayload["text"])
	}
	if got, _ := wsPayload["previous_response_id"].(string); got != "resp-123" {
		t.Fatalf("ws previous_response_id=%q want resp-123", got)
	}
	if got, ok := wsPayload["generate"].(bool); !ok || got {
		t.Fatalf("ws generate=%v want false", wsPayload["generate"])
	}
}
