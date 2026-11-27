package antigravity_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/adapter/cursor"
	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

type fixtureAuth struct{}

func (fixtureAuth) Token(context.Context) (string, error) {
	return "fixture-access-token", nil
}

func TestProviderExecutesResolvedGeminiRequest(t *testing.T) {
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Authorization") != "Bearer fixture-access-token" {
				t.Error("request did not use the injected access token")
			}

			switch request.URL.Path {
			case "/v1internal:loadCodeAssist":
				_, _ = io.WriteString(
					writer,
					`{"currentTier":{"id":"fixture"},"cloudaicompanionProject":"fixture-project"}`,
				)
			case "/v1internal:streamGenerateContent":
				var envelope antigravity.CodeAssistEnvelope
				if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
					t.Errorf("decode generation request: %v", err)
				}

				if envelope.Project != "fixture-project" || envelope.Model != "gemini-test" ||
					envelope.Request.SessionID != "conversation" ||
					!strings.HasPrefix(envelope.RequestID, "agent/conversation/1000/") {
					t.Errorf("unexpected generation request: %+v", envelope)
				}

				_, _ = io.WriteString(
					writer,
					"data: "+
						`{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}]}}`+"\n",
				)
			default:
				t.Errorf("unexpected request path %s", request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
			}
		}),
	)
	defer server.Close()

	backend, err := antigravity.NewProvider(provider.Deps{
		Config: config.AdapterConfig{
			Antigravity: config.AdapterAntigravity{
				BaseURL:       server.URL,
				ClientVersion: "1.0.0",
			},
		},
		Auth:       fixtureAuth{},
		HTTPClient: server.Client(),
		Now:        func() time.Time { return time.Unix(1, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}

	var events streamRecorder
	result, err := backend.Execute(
		t.Context(),
		resolver.ResolvedRequest{
			Model:  "gemini-test",
			Cursor: cursor.Request{ConversationID: "conversation"},
			OpenAI: openai.ChatRequest{
				Messages: []openai.ChatMessage{
					{Role: "user", Content: json.RawMessage(`"hello"`)},
				},
			},
		},
		&events,
	)
	if err != nil {
		t.Fatal(err)
	}

	if result.FinishReason != "stop" || len(events) != 1 {
		t.Fatalf("finish = %q, events = %#v", result.FinishReason, events)
	}

	event, ok := events[0].(render.TextDelta)
	if !ok || event.Text != "answer" {
		t.Fatalf("event = %#v, want answer text", events[0])
	}
}
