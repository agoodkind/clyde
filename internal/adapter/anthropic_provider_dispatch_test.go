package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

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
	status, body := aerr.HTTPStatus, aerr.openAIErrorBody()

	// This intentionally does not preserve HTTP 429 or OpenAI
	// rate_limit_error on the Cursor/OpenAI surface. Cursor treats that
	// pair as a user-provided API-key failure and replaces Clyde's real
	// Anthropic message with fallback chrome. The contract here is the
	// Codex context-limit style: invalid_request_error with the actual
	// message in error.message, while the original 429 remains available
	// to logs and diagnostics through UpstreamStatus.
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d want %d", status, http.StatusBadRequest)
	}
	if body.Type != "invalid_request_error" || body.Code != "upstream_rate_limited" {
		t.Fatalf("body=%+v", body)
	}
	if body.Message != "rate limit reached" {
		t.Fatalf("message=%q", body.Message)
	}
	if aerr.UpstreamStatus != http.StatusTooManyRequests {
		t.Fatalf("upstream status=%d want %d", aerr.UpstreamStatus, http.StatusTooManyRequests)
	}
}

func TestAnthropicProviderErrorResponseMapsWrappedTransportFailure(t *testing.T) {
	t.Parallel()

	upstreamErr := &anthropic.UpstreamError{
		Classification: anthropic.Classify(nil, errors.New("connection reset")),
		Cause:          errors.New("connection reset"),
	}
	aerr := anthropicProviderAdapterError(errors.Join(errors.New("collect failed"), upstreamErr))
	status, body := aerr.HTTPStatus, aerr.openAIErrorBody()

	if status != http.StatusBadGateway {
		t.Fatalf("status=%d want %d", status, http.StatusBadGateway)
	}
	if body.Type != "server_error" || body.Code != "upstream_unavailable" {
		t.Fatalf("body=%+v", body)
	}
	if body.Message == "" {
		t.Fatalf("message must not be empty")
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
	srv, _ := newLoggingServer(t, config.LoggingConfig{Body: config.LoggingBody{Mode: "off"}})
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
