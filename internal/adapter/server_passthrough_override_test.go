package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/config"
)

func TestRedactedHeader(t *testing.T) {
	cases := []struct {
		name     string
		header   string
		expected bool
	}{
		{name: "authorization", header: "authorization", expected: true},
		{name: "proxy-authorization", header: "proxy-authorization", expected: true},
		{name: "cookie", header: "cookie", expected: true},
		{name: "set-cookie", header: "set-cookie", expected: true},
		{name: "x-clyde-token", header: "x-clyde-token", expected: true},
		{name: "x-cursor-session", header: "x-cursor-session", expected: true},
		{name: "x-cursor-version", header: "x-cursor-version", expected: true},
		{name: "openai-api-key", header: "openai-api-key", expected: true},
		{name: "openai-organization", header: "openai-organization", expected: true},
		{name: "chatgpt-account-id", header: "chatgpt-account-id", expected: true},
		{name: "x-amz-security-token", header: "x-amz-security-token", expected: true},
		{name: "x-custom-api-key", header: "x-custom-api-key", expected: true},
		{name: "content-type", header: "content-type", expected: false},
		{name: "x-request-id", header: "x-request-id", expected: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactedHeader(tc.header)
			if got != tc.expected {
				t.Fatalf("redactedHeader(%q) = %v want %v", tc.header, got, tc.expected)
			}
		})
	}
}

func TestRedactedHeaders(t *testing.T) {
	headers := map[string][]string{
		"Authorization":        {"redactme"},
		"Content-Type":         {"application/json"},
		"X-AMZ-Security-Token": {"redactme"},
		"X-Cursor-Secret":      {"value"},
		"OpenAI-Token":         {"value"},
		"Chatgpt-Account-Id":   {"value"},
		"User-Agent":           {"ua"},
	}

	out := redactedHeaders(headers)
	expected := map[string]string{
		"authorization":        "[redacted]",
		"x-amz-security-token": "[redacted]",
		"content-type":         "application/json",
		"x-cursor-secret":      "[redacted]",
		"openai-token":         "[redacted]",
		"chatgpt-account-id":   "[redacted]",
		"user-agent":           "ua",
	}
	if !reflect.DeepEqual(out, expected) {
		t.Fatalf("redactedHeaders = %#v want %#v", out, expected)
	}
}

func TestPassthroughOverrideWrapsMalformedUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "local backend failed")
	}))
	defer upstream.Close()

	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local-model","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	// Cursor BYOK never sees HTTP 5xx + server_error from a provider
	// failure; the boundary flips upstream non-2xx into a parseable
	// invalid_request_error so the upstream message renders in the
	// chat transcript instead of triggering Cursor's BYOK fallback.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if out.Error.Type != "invalid_request_error" {
		t.Fatalf("error type = %+v", out.Error)
	}
	if !strings.Contains(out.Error.Message, "local backend failed") {
		t.Fatalf("message = %q, want upstream body included", out.Error.Message)
	}
}

func TestPassthroughOverridePreservesOpenAIErrorEnvelope(t *testing.T) {
	const upstreamBody = `{"error":{"message":"rate limit from upstream","type":"rate_limit_error","code":"rate_limit_exceeded","param":"model"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	srv := newPassthroughOverrideTestServer(t, upstream.URL+"/v1")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"local-model","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	// Boundary flips upstream 429 into a Cursor-safe
	// invalid_request_error envelope so the upstream rate limit
	// message text renders in the chat transcript rather than the
	// generic Cursor rate-limit chrome.
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if out.Error.Type != "invalid_request_error" || out.Error.Code != "upstream_rate_limited" {
		t.Fatalf("error = %+v", out.Error)
	}
	if !strings.Contains(out.Error.Message, "rate limit from upstream") {
		t.Fatalf("message = %q, want upstream body text preserved", out.Error.Message)
	}
}

func TestMutatePassthroughOverrideRequestBodyUsesResolvedWireEffort(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wireEffort    string
		wantTopLevel  string
		wantNested    string
		wantTopAbsent bool
	}{
		{name: "top level", body: `{"reasoning_effort":"ultra"}`, wireEffort: "max", wantTopLevel: "max"},
		{name: "nested", body: `{"reasoning":{"effort":"ultra","summary":"auto"}}`, wireEffort: "max", wantNested: "max", wantTopAbsent: true},
		{name: "nested null", body: `{"reasoning":null}`, wireEffort: "max", wantNested: "max", wantTopAbsent: true},
		{name: "omitted", body: `{}`, wireEffort: "max", wantTopLevel: "max"},
		{name: "both", body: `{"reasoning_effort":"ultra","reasoning":{"effort":"ultra"}}`, wireEffort: "max", wantTopLevel: "max", wantNested: "max"},
		{name: "unset wire effort", body: `{"reasoning_effort":"ultra"}`, wireEffort: "", wantTopLevel: "ultra"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, rewritten, _ := mutatePassthroughOverrideRequestBody(
				[]byte(test.body),
				"",
				test.wireEffort,
				false,
			)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rewritten, &fields); err != nil {
				t.Fatalf("decode rewritten body: %v", err)
			}
			if test.wantTopAbsent {
				if _, ok := fields["reasoning_effort"]; ok {
					t.Fatalf("reasoning_effort unexpectedly added: %s", rewritten)
				}
			} else {
				var topLevel string
				if err := json.Unmarshal(fields["reasoning_effort"], &topLevel); err != nil {
					t.Fatalf("decode reasoning_effort: %v", err)
				}
				if topLevel != test.wantTopLevel {
					t.Fatalf("reasoning_effort = %q, want %q", topLevel, test.wantTopLevel)
				}
			}
			if test.wantNested != "" {
				var nested struct {
					Effort string `json:"effort"`
				}
				if err := json.Unmarshal(fields["reasoning"], &nested); err != nil {
					t.Fatalf("decode reasoning: %v", err)
				}
				if nested.Effort != test.wantNested {
					t.Fatalf("reasoning.effort = %q, want %q", nested.Effort, test.wantNested)
				}
			}
		})
	}
}

func TestExactPassthroughOverrideForwardsConfiguredWireEffort(t *testing.T) {
	var captured map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	tools := true
	vision := false
	cfg := baseConfig()
	cfg.Enabled = true
	if cfg.ModelProfiles == nil {
		cfg.ModelProfiles = make(map[string]config.AdapterModelProfile)
	}
	if cfg.PassthroughOverrides == nil {
		cfg.PassthroughOverrides = make(map[string]config.AdapterPassthroughOverride)
	}
	if cfg.Models == nil {
		cfg.Models = make(map[string]config.AdapterModelDeclaration)
	}
	cfg.ModelProfiles["mapped-passthrough"] = config.AdapterModelProfile{
		Contexts:         []config.AdapterModelProfileContext{{Name: "standard", Tokens: 100000}},
		MaxOutputTokens:  16000,
		ReasoningEfforts: []config.AdapterReasoningEffort{"max", "ultra"},
		ReasoningEffortWireValues: map[config.AdapterReasoningEffort]config.AdapterReasoningEffort{
			"ultra": "max",
		},
		DefaultEffort:  "max",
		SupportsTools:  &tools,
		SupportsVision: &vision,
	}
	cfg.PassthroughOverrides["mapped"] = config.AdapterPassthroughOverride{BaseURL: upstream.URL + "/v1"}
	cfg.Models["gpt-mapped"] = config.AdapterModelDeclaration{
		Provider:            config.AdapterModelProviderPassthroughOverride,
		WireModel:           "gpt-upstream",
		Profile:             "mapped-passthrough",
		PassthroughOverride: "mapped",
	}
	srv, err := New(
		context.Background(),
		cfg,
		config.LoggingConfig{},
		Deps{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-mapped","reasoning_effort":"ultra","messages":[{"role":"user","content":"hello"}]}`),
	)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var effort string
	if err := json.Unmarshal(captured["reasoning_effort"], &effort); err != nil {
		t.Fatalf("decode captured reasoning effort: %v", err)
	}
	if effort != "max" {
		t.Fatalf("upstream reasoning_effort = %q, want max", effort)
	}
}

func newPassthroughOverrideTestServer(t *testing.T, baseURL string) *Server {
	t.Helper()
	cfg := baseConfig()
	cfg.Enabled = true
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{
		BaseURL: baseURL,
	}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}
