package adapter

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
)

func TestProviderBackedHandlersEmitOneLifecycle(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = true
	var events []adapterruntime.RequestEvent
	server, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		RequestEvents: func(_ context.Context, event adapterruntime.RequestEvent) {
			events = append(events, event)
		},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	provider := anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		Prepare: func(_ context.Context, resolved adapterresolver.ResolvedRequest, requestID string) (anthropic.PreparedRequest, error) {
			prepared := resolved
			return anthropic.PreparedRequest{
				Request: anthropic.Request{
					Model: resolved.Model, Stream: resolved.OpenAI.Stream,
				},
				Resolved: &prepared, RequestID: requestID, Stream: resolved.OpenAI.Stream,
			}, nil
		},
		ExecutePrepared: func(_ context.Context, prepared anthropic.PreparedRequest, writer adapterprovider.EventWriter) (adapterprovider.Result, error) {
			if prepared.NativeIngress {
				nativeWriter, ok := writer.(*nativeAnthropicStreamWriter)
				if !ok {
					t.Fatalf("native writer type = %T", writer)
				}
				nativeWriter.commit(http.Header{"Content-Type": {"text/event-stream"}})
				if writeErr := nativeWriter.write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")); writeErr != nil {
					t.Fatalf("native stream write: %v", writeErr)
				}
			} else if writeErr := writer.WriteEvent(adapterrender.TextDelta{Text: "ok"}); writeErr != nil {
				t.Fatalf("provider event write: %v", writeErr)
			}
			return adapterprovider.Result{FinishReason: "stop"}, nil
		},
	})
	server.anthropicProvider = provider
	server.providerRegistry.Register(provider)

	testCases := []struct {
		name string
		path string
		body string
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"claude-future","messages":[{"role":"user","content":"hello"}],"stream":true}`},
		{name: "native", path: "/v1/messages", body: `{"model":"claude-future","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":true}`},
		{name: "responses", path: "/v1/responses", body: `{"model":"claude-future","input":"hello","stream":true}`},
	}
	executionIDs := make(map[string]bool)
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			startIndex := len(events)
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
			request.Header.Set(clydeingress.HeaderRequestID, "reused-caller-id")
			response := httptest.NewRecorder()
			server.mux.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			requestEvents := events[startIndex:]
			assertSingleStreamingLifecycle(t, requestEvents)
			executionID := requestEvents[0].ExecutionID
			if executionIDs[executionID] {
				t.Fatalf("execution_id %q was reused", executionID)
			}
			executionIDs[executionID] = true
		})
	}
}

func assertSingleStreamingLifecycle(t *testing.T, events []adapterruntime.RequestEvent) {
	t.Helper()
	wantStages := []adapterruntime.RequestStage{
		adapterruntime.RequestStageStarted,
		adapterruntime.RequestStageStreamOpened,
		adapterruntime.RequestStageCompleted,
	}
	if len(events) != len(wantStages) {
		t.Fatalf("events = %+v, want %d lifecycle events", events, len(wantStages))
	}
	executionID := events[0].ExecutionID
	if executionID == "" {
		t.Fatal("execution_id is empty")
	}
	for i, event := range events {
		if event.Stage != wantStages[i] {
			t.Fatalf("event %d stage = %q, want %q", i, event.Stage, wantStages[i])
		}
		if event.ExecutionID != executionID {
			t.Fatalf("event %d execution_id = %q, want %q", i, event.ExecutionID, executionID)
		}
	}
}
