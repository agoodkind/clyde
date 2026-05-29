package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"goodkind.io/clyde/codexwire"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// codexHTTPSSEBodyForTest builds an SSE body matching the codex event
// shape the parser consumes. event lines carry the event type and data
// lines carry the JSON payload, terminated with a blank line.
func codexHTTPSSEBodyForTest(t *testing.T) string {
	t.Helper()
	events := []map[string]any{
		{
			"type":     "response.created",
			"response": map[string]any{"id": "resp-http-1"},
		},
		{
			"type":  "response.output_text.delta",
			"delta": "hello",
		},
		{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp-http-1",
				"usage": map[string]any{
					"input_tokens":          5,
					"output_tokens":         1,
					"total_tokens":          6,
					"input_tokens_details":  map[string]any{"cached_tokens": 0},
					"output_tokens_details": map[string]any{"reasoning_tokens": 0},
				},
			},
		},
	}
	var b strings.Builder
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		eventType, _ := event["type"].(string)
		b.WriteString("event: ")
		b.WriteString(eventType)
		b.WriteString("\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	return b.String()
}

func TestRunHTTPTransportEventsParsesSSEAndCompletes(t *testing.T) {
	t.Parallel()

	var sawStreamTrue atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		if accept := r.Header.Get("Accept"); accept != "text/event-stream" {
			t.Errorf("Accept=%q want text/event-stream", accept)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type=%q want application/json", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		streamed, _ := body["stream"].(bool)
		if !streamed {
			t.Errorf("body.stream=%v want true", body["stream"])
		}
		// The HTTP body must NOT carry the websocket envelope `type`
		// field. That field belongs to ResponseCreateWsRequest only.
		if _, hasType := body["type"]; hasType {
			t.Errorf("HTTP body must not include the websocket 'type' field")
		}
		sawStreamTrue.Store(true)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(codexHTTPSSEBodyForTest(t)))
	}))
	defer server.Close()

	var emittedCount atomic.Int32
	result, err := runHTTPTransportEvents(context.Background(), HTTPTransportConfig{
		URL:        server.URL,
		HTTPClient: server.Client(),
		Token:      "test-token",
		RequestID:  "req-http",
	}, HTTPTransportRequest{
		Model: "gpt-5.4",
		Input: []codexwire.InputItem{{
			Type:    codexwire.ItemTypeMessage,
			Role:    "user",
			Content: codexwire.ContentItems{{Type: codexwire.ContentItemInputText, Text: "hi"}},
		}},
	}, func(adapterrender.Event) error {
		emittedCount.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("runHTTPTransportEvents err=%v", err)
	}
	if !sawStreamTrue.Load() {
		t.Fatalf("server did not see stream:true in the HTTP body")
	}
	if result.ResponseID != "resp-http-1" {
		t.Fatalf("ResponseID=%q want resp-http-1", result.ResponseID)
	}
	if emittedCount.Load() == 0 {
		t.Fatalf("no events emitted")
	}
}

func TestRunHTTPTransportEventsFoldsUpstreamStatusIntoErrorMessage(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusTooManyRequests,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("upstream-non200-canary"))
		}))
		_, err := runHTTPTransportEvents(context.Background(), HTTPTransportConfig{
			URL:        server.URL,
			HTTPClient: server.Client(),
			RequestID:  "req-http-err",
		}, HTTPTransportRequest{Model: "gpt-5.4"}, func(adapterrender.Event) error {
			return nil
		})
		server.Close()
		if err == nil {
			t.Fatalf("status=%d err=nil want error", status)
		}
		// The status is preserved in the error message text so the
		// Cursor-facing error.message carries diagnostics. The status
		// must NOT propagate as an HTTP status code; the codex provider
		// error boundary always maps codex failures to HTTP 400 +
		// invalid_request_error + upstream_*.
		if !strings.Contains(err.Error(), "upstream status") {
			t.Fatalf("status=%d err=%q missing 'upstream status' text", status, err.Error())
		}
		if !strings.Contains(err.Error(), "upstream-non200-canary") {
			t.Fatalf("status=%d err=%q missing body canary", status, err.Error())
		}
	}
}

func TestRunHTTPTransportEventsRefreshesOn401AndRetriesOnce(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	var lastBearer atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBearer.Store(r.Header.Get("Authorization"))
		n := requestCount.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_token"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(codexHTTPSSEBodyForTest(t)))
	}))
	defer server.Close()

	var refreshCalls atomic.Int32
	_, err := runHTTPTransportEvents(context.Background(), HTTPTransportConfig{
		URL:        server.URL,
		HTTPClient: server.Client(),
		Token:      "stale-token",
		RequestID:  "req-http-401",
		AuthRefresh: func(_ context.Context) (string, error) {
			refreshCalls.Add(1)
			return "fresh-token", nil
		},
	}, HTTPTransportRequest{
		Model: "gpt-5.4",
		Input: []codexwire.InputItem{{
			Type:    codexwire.ItemTypeMessage,
			Role:    "user",
			Content: codexwire.ContentItems{{Type: codexwire.ContentItemInputText, Text: "hi"}},
		}},
	}, func(adapterrender.Event) error { return nil })
	if err != nil {
		t.Fatalf("runHTTPTransportEvents err=%v", err)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls=%d want 1", refreshCalls.Load())
	}
	if requestCount.Load() != 2 {
		t.Fatalf("server requests=%d want 2", requestCount.Load())
	}
	if bearer, _ := lastBearer.Load().(string); bearer != "Bearer fresh-token" {
		t.Fatalf("last request bearer=%q want fresh-token", bearer)
	}
}
