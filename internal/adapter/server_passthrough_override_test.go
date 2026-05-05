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
		"User-Agent":           {"ua"},
	}

	out := redactedHeaders(headers)
	expected := map[string]string{
		"authorization":        "[redacted]",
		"x-amz-security-token": "[redacted]",
		"content-type":         "application/json",
		"x-cursor-secret":      "[redacted]",
		"openai-token":         "[redacted]",
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

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadGateway)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	var out ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rec.Body.String())
	}
	if out.Error.Type != "server_error" || out.Error.Code != "upstream_failed" {
		t.Fatalf("error = %+v", out.Error)
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

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != upstreamBody {
		t.Fatalf("body = %s, want passthrough %s", got, upstreamBody)
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
