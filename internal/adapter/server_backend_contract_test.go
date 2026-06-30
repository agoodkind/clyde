package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

// directOAuthConfig returns a baseConfig variant that enables both the
// Anthropic OAuth provider (via DirectOAuth) and the Codex provider so
// the server registers both in its provider.Registry at construction.
func directOAuthConfig() config.AdapterConfig {
	cfg := baseConfig()
	cfg.DirectOAuth = true
	cfg.Anthropic.OAuth = config.AdapterOAuth{
		MessagesURL:      "https://example.invalid/v1/messages",
		AnthropicBeta:    "beta",
		AnthropicVersion: "2023-06-01",
	}
	cfg.Codex.Enabled = true
	cfg.Codex.AuthFile = "~/.codex/auth.json"
	cfg.Codex.Models = testCodexModels()
	return cfg
}

// TestServerRegistersAnthropicAndCodexProviders locks in that, after
// server construction with both backends enabled, the provider.Registry
// contains both the Anthropic and Codex provider identities. This is the
// instance-registry contract the dispatcher's Lookup depends on.
func TestServerRegistersAnthropicAndCodexProviders(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{}, func(cfg *config.AdapterConfig) {
		*cfg = directOAuthConfig()
	})

	ids := srv.providerRegistry.IDs()
	if !slices.Contains(ids, adapterresolver.ProviderAnthropic) {
		t.Fatalf("registry IDs %v missing ProviderAnthropic", ids)
	}
	if !slices.Contains(ids, adapterresolver.ProviderCodex) {
		t.Fatalf("registry IDs %v missing ProviderCodex", ids)
	}
}

// TestDispatchUnknownBackendReturnsUnsupportedBackend locks in that a
// resolved backend that is not a known provider family returns the typed
// unsupported-backend adapter error through the OpenAI error boundary.
func TestDispatchUnknownBackendReturnsUnsupportedBackend(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

	if ids := srv.providerRegistry.IDs(); len(ids) != 0 {
		t.Fatalf("expected empty registry for baseConfig, got %v", ids)
	}

	unknownBackend := adaptermodel.BackendID("mystery")
	out, status := dispatchToErrorEnvelope(t, srv, dispatchArgs{
		alias:    "mystery-model",
		provider: unknownBackend,
	})

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if out.Error.Type != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", out.Error.Type)
	}
	if out.Error.Code != "unsupported_backend" {
		t.Fatalf("error code = %q, want unsupported_backend", out.Error.Code)
	}
	if !strings.Contains(out.Error.Message, "unsupported backend") {
		t.Fatalf("error message = %q, want it to mention unsupported backend", out.Error.Message)
	}
}

// TestDispatchKnownBackendWithoutRegisteredProviderReturnsUpstreamUnavailable
// locks in that a known provider family with no registered provider (the
// upstream is not enabled in config) returns the typed upstream-unavailable
// adapter error, preserving the pre-registry behavior where a disabled
// codex backend reported upstream_unavailable rather than unsupported.
func TestDispatchKnownBackendWithoutRegisteredProviderReturnsUpstreamUnavailable(t *testing.T) {
	t.Parallel()
	srv, _ := newLoggingServer(t, config.LoggingConfig{})

	if ids := srv.providerRegistry.IDs(); len(ids) != 0 {
		t.Fatalf("expected empty registry for baseConfig, got %v", ids)
	}

	out, status := dispatchToErrorEnvelope(t, srv, dispatchArgs{
		alias:    "gpt-5.4",
		provider: adapterresolver.ProviderCodex,
	})

	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if out.Error.Type != "invalid_request_error" {
		t.Fatalf("error type = %q, want invalid_request_error", out.Error.Type)
	}
	if out.Error.Code != "upstream_unavailable" {
		t.Fatalf("error code = %q, want upstream_unavailable", out.Error.Code)
	}
}

// dispatchArgs carries the per-test inputs for dispatchToErrorEnvelope so
// the helper stays a single typed entry point. The resolver is now the
// authoritative single resolution path: resolution failure is handled in
// handleChat before dispatch (covered by server_dispatch_logging_test.go),
// so the dispatch-time error cases are unknown-backend and
// known-family-without-registered-provider, both keyed off the resolved
// request's Provider.
type dispatchArgs struct {
	alias    string
	provider adapterresolver.ProviderID
}

// dispatchToErrorEnvelope drives dispatchResolvedChat for an error-path
// request and decodes the rendered OpenAI error envelope. The request is
// posted to /v1/chat/completions so the OpenAI route family's renderer is
// selected by the error boundary.
func dispatchToErrorEnvelope(t *testing.T, srv *Server, args dispatchArgs) (adapteropenai.ErrorResponse, int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	chatReq := ChatRequest{Model: args.alias}
	resolvedReq := adapterresolver.ResolvedRequest{Provider: args.provider, Alias: args.alias}

	srv.dispatchResolvedChat(
		rec,
		req,
		chatReq,
		"",
		"req-test",
		[]byte(`{}`),
		ingresscontract.IngressContext{},
		resolvedReq,
	)

	var out adapteropenai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal error envelope: %v; body=%s", err, rec.Body.String())
	}
	return out, rec.Code
}
