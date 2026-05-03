package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

type fakeAuth struct {
	token string
	err   error
}

func (f fakeAuth) Token(_ context.Context) (string, error) {
	return f.token, f.err
}

type capturingWriter struct {
	events  []adapterrender.Event
	flushed bool
}

func (c *capturingWriter) WriteEvent(ev adapterrender.Event) error {
	c.events = append(c.events, ev)
	return nil
}

func (c *capturingWriter) Flush() error {
	c.flushed = true
	return nil
}

func TestProviderID(t *testing.T) {
	p := NewProvider(adapterprovider.Deps{}, ProviderOptions{})
	if got := p.ID(); got != adapterresolver.ProviderCodex {
		t.Fatalf("ID() = %v, want %v", got, adapterresolver.ProviderCodex)
	}
}

func TestProviderExecuteNilAuthReturnsAuthMissing(t *testing.T) {
	p := NewProvider(adapterprovider.Deps{}, ProviderOptions{})
	w := &capturingWriter{}
	_, err := p.Execute(context.Background(), adapterresolver.ResolvedRequest{}, w)
	if !errors.Is(err, adapterprovider.ErrAuthMissing) {
		t.Fatalf("Execute() err = %v, want ErrAuthMissing", err)
	}
}

func TestProviderExecuteEmptyTokenReturnsAuthMissing(t *testing.T) {
	p := NewProvider(adapterprovider.Deps{Auth: fakeAuth{token: "  "}}, ProviderOptions{})
	w := &capturingWriter{}
	_, err := p.Execute(context.Background(), adapterresolver.ResolvedRequest{}, w)
	if !errors.Is(err, adapterprovider.ErrAuthMissing) {
		t.Fatalf("Execute() err = %v, want ErrAuthMissing", err)
	}
}

func TestProviderExecuteAuthErrorSurfaces(t *testing.T) {
	want := errors.New("auth boom")
	p := NewProvider(adapterprovider.Deps{Auth: fakeAuth{err: want}}, ProviderOptions{})
	w := &capturingWriter{}
	_, err := p.Execute(context.Background(), adapterresolver.ResolvedRequest{}, w)
	if !errors.Is(err, want) {
		t.Fatalf("Execute() err = %v, want %v", err, want)
	}
}

func TestProviderExecuteInjectsUsageWarningBeforeAssistantText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = w.Write([]byte(`{
				"rate_limit": {
					"secondary_window": {
						"used_percent": 80,
						"limit_window_seconds": 604800,
						"reset_at": 1735689600
					}
				}
			}`))
		case "/backend-api/codex/responses":
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Fatalf("read request: %v", err)
			}
			if err := conn.WriteMessage(1, []byte(`{"type":"response.output_text.delta","delta":"answer"}`)); err != nil {
				t.Fatalf("write delta: %v", err)
			}
			if err := conn.WriteMessage(1, []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
				t.Fatalf("write complete: %v", err)
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	deps := adapterprovider.Deps{
		Auth:       fakeAuth{token: "token-123"},
		HTTPClient: server.Client(),
		Now:        func() time.Time { return time.Unix(1735682400, 0).UTC() },
	}
	deps.Config.Codex.BaseURL = server.URL + "/backend-api/codex/responses"
	p := NewProvider(deps, ProviderOptions{AccountID: "acct-123"})
	w := &capturingWriter{}
	result, err := p.Execute(context.Background(), adapterresolver.ResolvedRequest{
		Model:  "gpt-5.4",
		Effort: adapterresolver.EffortMedium,
		OpenAI: adapteropenai.ChatRequest{
			Messages: []adapteropenai.ChatMessage{{
				Role:    "user",
				Content: json.RawMessage(`"hello"`),
			}},
		},
	}, w)
	if err != nil {
		t.Fatalf("Execute() err = %v", err)
	}
	if len(w.events) < 2 {
		t.Fatalf("events len=%d want at least 2", len(w.events))
	}
	if got := w.events[0].Text; got != formatUsageWarningNotice("Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown.") {
		t.Fatalf("warning event=%q", got)
	}
	if got := w.events[1].Text; got != "answer" {
		t.Fatalf("assistant event=%q", got)
	}
	if result.ReasoningSummary != "Heads up, you have less than 25% of your weekly limit left. Run /status for a breakdown." {
		t.Fatalf("reasoning summary=%q", result.ReasoningSummary)
	}
}

func TestProviderExecuteSkipsUsageWarningWhenConfiguredThresholdNotMet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = w.Write([]byte(`{
				"rate_limit": {
					"secondary_window": {
						"used_percent": 80,
						"limit_window_seconds": 604800,
						"reset_at": 1735689600
					}
				}
			}`))
		case "/backend-api/codex/responses":
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Fatalf("upgrade: %v", err)
			}
			defer conn.Close()
			if _, _, err := conn.ReadMessage(); err != nil {
				t.Fatalf("read request: %v", err)
			}
			if err := conn.WriteMessage(1, []byte(`{"type":"response.output_text.delta","delta":"answer"}`)); err != nil {
				t.Fatalf("write delta: %v", err)
			}
			if err := conn.WriteMessage(1, []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)); err != nil {
				t.Fatalf("write complete: %v", err)
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	deps := adapterprovider.Deps{
		Auth:       fakeAuth{token: "token-123"},
		HTTPClient: server.Client(),
		Now:        func() time.Time { return time.Unix(1735682400, 0).UTC() },
	}
	deps.Config.Notices.Usage.ThresholdsUsedPercent = []float64{90}
	deps.Config.Codex.BaseURL = server.URL + "/backend-api/codex/responses"
	p := NewProvider(deps, ProviderOptions{AccountID: "acct-123"})
	w := &capturingWriter{}
	result, err := p.Execute(context.Background(), adapterresolver.ResolvedRequest{
		Model:  "gpt-5.4",
		Effort: adapterresolver.EffortMedium,
		OpenAI: adapteropenai.ChatRequest{
			Messages: []adapteropenai.ChatMessage{{
				Role:    "user",
				Content: json.RawMessage(`"hello"`),
			}},
		},
	}, w)
	if err != nil {
		t.Fatalf("Execute() err = %v", err)
	}
	if len(w.events) != 2 {
		t.Fatalf("events len=%d want 2", len(w.events))
	}
	if got := w.events[0].Text; got != "answer" {
		t.Fatalf("assistant event=%q", got)
	}
	if got := w.events[1].Kind; got != adapterrender.EventReasoningFinished {
		t.Fatalf("final event kind=%q", got)
	}
	if got := w.events[1].Text; got != "" {
		t.Fatalf("final event text=%q", got)
	}
	if result.ReasoningSummary != "" {
		t.Fatalf("reasoning summary=%q", result.ReasoningSummary)
	}
}

func TestCodexBaseURLDefaults(t *testing.T) {
	if got := codexBaseURL(""); got != "https://chatgpt.com/backend-api/codex/responses" {
		t.Errorf("codexBaseURL(\"\") = %q, want default", got)
	}
	custom := "https://example.com/codex"
	if got := codexBaseURL(custom); got != custom {
		t.Errorf("codexBaseURL custom = %q, want %q", got, custom)
	}
}

func TestCodexWebsocketURLConversion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "wss://chatgpt.com/backend-api/codex/responses"},
		{"https://example.com/codex", "wss://example.com/codex"},
		{"http://localhost:8080/codex", "ws://localhost:8080/codex"},
		{"weird://something", "weird://something"},
	}
	for _, tc := range cases {
		if got := codexWebsocketURL(tc.in); got != tc.want {
			t.Errorf("codexWebsocketURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCodexRequestIDPrefersAdapterID(t *testing.T) {
	req := adapterresolver.ResolvedRequest{
		RequestID: "chatcmpl-adapter",
		Cursor: adaptercursor.Request{
			RequestID: "cursor-request",
		},
	}
	if got := codexRequestID(req); got != "chatcmpl-adapter" {
		t.Fatalf("codexRequestID() = %q, want adapter id", got)
	}
}

func TestCodexRequestIDFallsBackToCursorID(t *testing.T) {
	req := adapterresolver.ResolvedRequest{
		Cursor: adaptercursor.Request{
			RequestID: "cursor-request",
		},
	}
	if got := codexRequestID(req); got != "cursor-request" {
		t.Fatalf("codexRequestID() = %q, want cursor id", got)
	}
}

func TestResolvedModelFromRequestPopulatesCodexFields(t *testing.T) {
	req := adapterresolver.ResolvedRequest{
		Provider:     adapterresolver.ProviderCodex,
		Family:       "gpt-5",
		Model:        "gpt-5.3-codex",
		Effort:       adapterresolver.EffortHigh,
		Instructions: "model base instructions",
		ContextBudget: adapterresolver.ContextBudget{
			InputTokens:  200000,
			OutputTokens: 16384,
		},
	}
	rm := resolvedModelFromRequest(req)
	if rm.Alias != "gpt-5.3-codex" {
		t.Errorf("Alias = %q", rm.Alias)
	}
	if rm.ClaudeModel != "gpt-5.3-codex" {
		t.Errorf("ClaudeModel = %q", rm.ClaudeModel)
	}
	if rm.Context != 200000 {
		t.Errorf("Context = %d", rm.Context)
	}
	if rm.MaxOutputTokens != 16384 {
		t.Errorf("MaxOutputTokens = %d", rm.MaxOutputTokens)
	}
	if rm.Effort != "high" {
		t.Errorf("Effort = %q", rm.Effort)
	}
	if rm.FamilySlug != "gpt-5" {
		t.Errorf("FamilySlug = %q", rm.FamilySlug)
	}
	if rm.Instructions != "model base instructions" {
		t.Errorf("Instructions = %q", rm.Instructions)
	}
}
