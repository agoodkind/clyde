package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

func TestAnthropicProviderErrorMapsClaudeAuthFailureToActionableInstruction(t *testing.T) {
	t.Parallel()

	authErr := &anthropic.AuthCredentialError{
		Message: "no usable Claude login credentials found; run `claude auth login`",
		Summary: []string{"credentials_file:present=false:access=false:refresh=false:expired=false:fingerprint=:error="},
	}
	wrapped := fmt.Errorf("oauth token: %w", authErr)
	aerr := anthropicProviderAdapterError(wrapped)

	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
	}
	if aerr.Code != "upstream_auth_failed" {
		t.Fatalf("code=%q want upstream_auth_failed", aerr.Code)
	}
	if aerr.Class != adapterErrorUpstreamAuthFailed {
		t.Fatalf("class=%q want upstream_auth_failed", aerr.Class)
	}
	if !strings.Contains(aerr.Message, "claude auth login") {
		t.Fatalf("message missing re-auth command: %q", aerr.Message)
	}

	env := renderedOpenAIEnvelope(t, aerr)
	if env.Error.Type != "invalid_request_error" {
		t.Fatalf("rendered type=%q want invalid_request_error", env.Error.Type)
	}
	if env.Error.Code != "upstream_auth_failed" {
		t.Fatalf("rendered code=%q want upstream_auth_failed", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "claude auth login") {
		t.Fatalf("rendered message missing re-auth command: %q", env.Error.Message)
	}
}

func TestPrepareAnthropicProviderRequestPreservesOpenAIStreamIntent(t *testing.T) {
	t.Parallel()

	server := &Server{
		anthr: anthropic.New(nil, nil, anthropic.Config{
			UserAgent:          "claude-cli/2.1.123",
			SystemPromptPrefix: "You are Claude Code.",
			CCVersion:          "2.1.123",
			CCEntrypoint:       "sdk-cli",
		}),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	resolved := adapterresolver.ResolvedRequest{
		Model:  "claude-sonnet-4-6",
		Effort: adapterresolver.EffortMedium,
		OpenAI: adapteropenai.ChatRequest{
			Model:  "clyde-sonnet-4.6-medium-thinking",
			Stream: true,
			Messages: []adapteropenai.ChatMessage{{
				Role:    "user",
				Content: []byte(`"Say ok."`),
			}},
		},
		ContextBudget: adapterresolver.ContextBudget{InputTokens: 200000, OutputTokens: 64000},
	}

	prepared, err := server.prepareAnthropicProviderRequest(context.Background(), resolved, "req-stream")
	if err != nil {
		t.Fatalf("prepareAnthropicProviderRequest() error = %v", err)
	}
	if !prepared.Stream {
		t.Fatalf("prepared.Stream = false, want true")
	}
	if prepared.Request.Stream {
		t.Fatalf("prepared.Request.Stream = true, want false before execution")
	}
}

func TestAnthropicProviderErrorResponseMapsUpstreamRateLimitToCursorSafeInvalidRequest(t *testing.T) {
	t.Parallel()

	upstreamErr := &anthropic.UpstreamError{
		Classification: anthropic.Classify(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, nil),
		Status:         http.StatusTooManyRequests,
		Message:        "rate limit reached",
	}
	aerr := anthropicProviderAdapterError(upstreamErr)

	// This intentionally does not preserve HTTP 429 or OpenAI
	// rate_limit_error on the Cursor/OpenAI surface. Cursor treats that
	// pair as a user-provided API-key failure and replaces Clyde's real
	// Anthropic message with fallback chrome. The contract here is the
	// Codex context-limit style: invalid_request_error with the actual
	// message in error.message, while the original 429 remains available
	// to logs and diagnostics through UpstreamStatus.
	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
	}
	if aerr.Code != "upstream_rate_limited" {
		t.Fatalf("adapter error=%+v", aerr)
	}
	if aerr.Message != "rate limit reached" {
		t.Fatalf("message=%q", aerr.Message)
	}
	if aerr.UpstreamStatus != http.StatusTooManyRequests {
		t.Fatalf("upstream status=%d want %d", aerr.UpstreamStatus, http.StatusTooManyRequests)
	}
	env := renderedOpenAIEnvelope(t, aerr)
	if env.Error.Type != "invalid_request_error" || env.Error.Code != "upstream_rate_limited" {
		t.Fatalf("rendered envelope=%+v", env.Error)
	}
}

// TestAnthropicProviderErrorResponseMapsSSEErrorEventToTypedClass
// pins the CLYDE-439 contract that an Anthropic SSE `event: error`
// frame on a 200 stream surfaces in the OpenAI route family envelope
// with the typed upstream class derived from the Anthropic envelope
// `error.type`, not the generic `upstream_failed`. The upstream HTTP
// status was 200; only the SSE error frame carries the routing
// signal. Verifies all five documented error.type values.
func TestAnthropicProviderErrorResponseMapsSSEErrorEventToTypedClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		kind     anthropic.ErrorKind
		wantCode string
	}{
		{name: "rate_limit", kind: anthropic.ErrorKindRateLimit, wantCode: "upstream_rate_limited"},
		{name: "overloaded", kind: anthropic.ErrorKindOverloaded, wantCode: "upstream_failed"},
		{name: "authentication", kind: anthropic.ErrorKindAuth, wantCode: "upstream_auth_failed"},
		{name: "invalid_request", kind: anthropic.ErrorKindInvalidRequest, wantCode: "invalid_request"},
		{name: "api", kind: anthropic.ErrorKindAPI, wantCode: "upstream_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstreamErr := &anthropic.UpstreamError{
				Classification: anthropic.Classification{Class: anthropic.ResponseClassFatalError},
				Status:         0,
				Message:        "Rate limited",
				ErrorType:      tc.kind,
			}
			aerr := anthropicProviderAdapterError(upstreamErr)
			if aerr.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
			}
			if aerr.Code != tc.wantCode {
				t.Fatalf("code=%q want %q", aerr.Code, tc.wantCode)
			}
			env := renderedOpenAIEnvelope(t, aerr)
			if env.Error.Type != "invalid_request_error" {
				t.Fatalf("rendered type=%q want invalid_request_error", env.Error.Type)
			}
			if env.Error.Code != tc.wantCode {
				t.Fatalf("rendered code=%q want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestAnthropicProviderErrorResponseMapsWrappedTransportFailure(t *testing.T) {
	t.Parallel()

	upstreamErr := &anthropic.UpstreamError{
		Classification: anthropic.Classify(nil, errors.New("connection reset")),
		Cause:          errors.New("connection reset"),
	}
	aerr := anthropicProviderAdapterError(errors.Join(errors.New("collect failed"), upstreamErr))

	// Cursor BYOK never sees HTTP 5xx + server_error; the boundary
	// flips wrapped transport failures into a Cursor-safe
	// invalid_request_error envelope.
	if aerr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", aerr.HTTPStatus, http.StatusBadRequest)
	}
	if aerr.Message == "" {
		t.Fatalf("message must not be empty")
	}
	env := renderedOpenAIEnvelope(t, aerr)
	if env.Error.Type != "invalid_request_error" {
		t.Fatalf("rendered envelope=%+v", env.Error)
	}
}

func TestAnthropicIngressProviderErrorPreservesNativeRateLimitShape(t *testing.T) {
	t.Parallel()

	upstreamErr := &anthropic.UpstreamError{
		Classification: anthropic.Classify(&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}, nil),
		Status:         http.StatusTooManyRequests,
		Message:        "too many requests",
	}
	rec := httptest.NewRecorder()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	srv.writeAnthropicIngressProviderError(rec, req, upstreamErr)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want %d", rec.Code, http.StatusTooManyRequests)
	}
	var envelope anthropic.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if envelope.Type != "error" {
		t.Fatalf("envelope type=%q want error", envelope.Type)
	}
	if envelope.Error.Type != "rate_limit_error" {
		t.Fatalf("error type=%q want rate_limit_error", envelope.Error.Type)
	}
	if envelope.Error.Message != "too many requests" {
		t.Fatalf("message=%q", envelope.Error.Message)
	}
}
