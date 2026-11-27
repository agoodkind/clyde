package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

type antigravityFixtureAuth struct{}

func (antigravityFixtureAuth) Token(context.Context) (string, error) {
	return "fixture-antigravity-token", nil
}

func newAntigravityRoutingServer(t *testing.T, upstream http.Handler) *Server {
	t.Helper()
	wire := httptest.NewServer(upstream)
	t.Cleanup(wire.Close)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(wire.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	cfg.Antigravity = config.AdapterAntigravity{
		Enabled:       true,
		BaseURL:       "http://" + net.JoinHostPort("localhost", port),
		ClientVersion: "1.2.3",
		ClientID:      "fixture-client",
		ClientSecret:  "fixture-secret",
		Project:       "fixture-project",
	}
	cfg.ModelRoutes = append(
		cfg.ModelRoutes,
		modelRouteForTest(
			"gemini-*",
			config.AdapterModelProviderAntigravity,
			config.AdapterIngressOpenAI,
			config.AdapterIngressCursor,
		),
	)
	srv, err := New(
		t.Context(),
		cfg,
		config.LoggingConfig{},
		Deps{
			GetAuth: func(id adapterresolver.ProviderID) adapterprovider.AuthLookup {
				if id == adapterresolver.ProviderAntigravity {
					return antigravityFixtureAuth{}
				}

				return nil
			},
			ReplyStore: testReplyStore(t),
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	return srv
}

func TestAntigravityGenericChatRestoresScopedToolSignature(t *testing.T) {
	const signature = "fixture-thought-signature"
	requests := make(chan string, 3)
	srv := newAntigravityRoutingServer(
		t,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read upstream request: %v", err)
			}

			requests <- string(body)
			_, _ = io.WriteString(
				w,
				"data: "+
					`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"fixture-call","name":"lookup","args":{"key":1}},"thoughtSignature":"`+
					signature+`"}]},"finishReason":"STOP"}]}}`+"\n\n",
			)
		}),
	)

	post := func(body []byte) ChatResponse {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), ingressLabelKey{}, "openai"))
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)

		var completion ChatResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil ||
			rec.Code != http.StatusOK || len(completion.Choices) != 1 {
			t.Fatalf("status = %d, body = %s, decode error = %v", rec.Code, rec.Body.String(), err)
		}

		return completion
	}

	first := post(
		[]byte(`{"model":"gemini-fixture","messages":[{"role":"user","content":"hello"}]}`),
	)
	firstWire := <-requests
	echo := first.Choices[0].Message
	if len(echo.ToolCalls) != 1 || !strings.Contains(string(echo.Content), "clyde-replay") {
		t.Fatalf("reply is missing the tool call or replay reference: %+v", echo)
	}

	var request ChatRequest
	request.Model = "gemini-fixture"
	request.Messages = []ChatMessage{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
		echo,
		{Role: "tool", ToolCallID: "fixture-call", Content: json.RawMessage(`"tool result"`)},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	post(body)
	secondWire := <-requests
	if !strings.Contains(secondWire, `"thoughtSignature":"`+signature+`"`) ||
		strings.Contains(secondWire, "clyde-replay") {
		t.Fatalf(
			"upstream did not receive the stored signature without the replay marker: %s",
			secondWire,
		)
	}

	var firstSession, secondSession struct {
		Request struct {
			SessionID string `json:"sessionId"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(firstWire), &firstSession); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal([]byte(secondWire), &secondSession); err != nil {
		t.Fatal(err)
	}

	if firstSession.Request.SessionID == "" ||
		secondSession.Request.SessionID != firstSession.Request.SessionID {
		t.Fatalf(
			"session IDs = %q and %q",
			firstSession.Request.SessionID,
			secondSession.Request.SessionID,
		)
	}

	request.Messages[1].ToolCalls[0].Function.Arguments = `{"key":2}`
	body, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	post(body)
	if wire := <-requests; strings.Contains(wire, `"thoughtSignature":"`+signature+`"`) {
		t.Fatal("adapter restored a signature after the tool call changed")
	}
}
