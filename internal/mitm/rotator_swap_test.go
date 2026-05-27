package mitm

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/oauthrotation/provider"
	"goodkind.io/clyde/internal/oauthrotation/ratelimitsink"
)

// stubRotatorSink is a deterministic RotatorSink that records its
// inputs and returns canned outputs. Tests assert on the recorded
// signals and call counts so behavior changes in the swap and observe
// helpers surface as test failures rather than silent passes.
type stubRotatorSink struct {
	mu sync.Mutex

	token         string
	account       provider.AccountID
	tokenErr      error
	tokenCalls    int
	lastTokenName provider.Name

	afterFailure        string
	afterFailureErr     error
	afterFailureCalls   int
	lastFailedToken     string
	lastAfterFailureCtx context.Context //nolint:containedctx // test-only capture

	observed     []ratelimitsink.Signal
	observeErr   error
	observeCalls int
}

func (s *stubRotatorSink) Token(_ context.Context, name provider.Name) (string, provider.AccountID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenCalls++
	s.lastTokenName = name
	return s.token, s.account, s.tokenErr
}

func (s *stubRotatorSink) TokenAfterAuthFailure(ctx context.Context, _ provider.Name, failed string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterFailureCalls++
	s.lastFailedToken = failed
	s.lastAfterFailureCtx = ctx
	return s.afterFailure, s.afterFailureErr
}

func (s *stubRotatorSink) Observe(_ context.Context, sig ratelimitsink.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observeCalls++
	s.observed = append(s.observed, sig)
	return s.observeErr
}

func (s *stubRotatorSink) snapshot() []ratelimitsink.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ratelimitsink.Signal, len(s.observed))
	copy(out, s.observed)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func newProbeRequest(t *testing.T, authHeader string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{}`))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

func TestSwapAuthorizationReplacesAnthropicBearer(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{token: "fresh-token", account: provider.AccountID("acct-1")}
	req := newProbeRequest(t, "Bearer sk-ant-oat01-original")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	if got != "fresh-token" {
		t.Fatalf("observeToken = %q, want fresh-token", got)
	}
	if !didSwap {
		t.Fatalf("didSwap = false, want true for /v1/messages with anthropic bearer")
	}
	if outbound := req.Header.Get("Authorization"); outbound != "Bearer fresh-token" {
		t.Fatalf("outbound Authorization = %q, want Bearer fresh-token", outbound)
	}
	if sink.tokenCalls != 1 {
		t.Fatalf("Token calls = %d, want 1", sink.tokenCalls)
	}
	if sink.lastTokenName != AnthropicProviderName {
		t.Fatalf("Token name = %q, want %q", sink.lastTokenName, AnthropicProviderName)
	}
}

func TestSwapAuthorizationLeavesNonAnthropicBearerAlone(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{token: "fresh-token"}
	req := newProbeRequest(t, "Bearer some-other-token")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	if got != "" {
		t.Fatalf("observeToken = %q, want empty for non-anthropic bearer", got)
	}
	if didSwap {
		t.Fatalf("didSwap = true, want false for non-anthropic bearer")
	}
	if outbound := req.Header.Get("Authorization"); outbound != "Bearer some-other-token" {
		t.Fatalf("outbound Authorization = %q, want unchanged", outbound)
	}
	if sink.tokenCalls != 0 {
		t.Fatalf("Token calls = %d, want 0", sink.tokenCalls)
	}
}

func TestSwapAuthorizationLeavesMissingAuthAlone(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{token: "fresh-token"}
	req := newProbeRequest(t, "")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	if got != "" {
		t.Fatalf("observeToken = %q, want empty when no auth header", got)
	}
	if didSwap {
		t.Fatalf("didSwap = true, want false when no auth header")
	}
	if outbound := req.Header.Get("Authorization"); outbound != "" {
		t.Fatalf("outbound Authorization = %q, want unset", outbound)
	}
	if sink.tokenCalls != 0 {
		t.Fatalf("Token calls = %d, want 0", sink.tokenCalls)
	}
}

func TestSwapAuthorizationNilSinkIsPassthrough(t *testing.T) {
	t.Parallel()
	req := newProbeRequest(t, "Bearer sk-ant-oat01-original")

	got, didSwap := swapAuthorizationForRotator(req, nil, discardLogger())

	// Nil sink still surfaces the inbound bearer as observeToken so callers
	// can pair the response with the inbound account, even though the
	// observe path no-ops on a nil sink.
	if got != "sk-ant-oat01-original" {
		t.Fatalf("observeToken = %q, want inbound bearer surfaced", got)
	}
	if didSwap {
		t.Fatalf("didSwap = true, want false with nil sink")
	}
	if outbound := req.Header.Get("Authorization"); outbound != "Bearer sk-ant-oat01-original" {
		t.Fatalf("outbound Authorization = %q, want unchanged", outbound)
	}
}

func TestSwapAuthorizationLeavesRequestAloneOnRotatorError(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{tokenErr: errStubBoom}
	req := newProbeRequest(t, "Bearer sk-ant-oat01-original")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	// Rotator error degrades to passthrough: observeToken carries the
	// inbound bearer so observation against the inbound slot still fires,
	// didSwap stays false so the 401 retry path stays disabled.
	if got != "sk-ant-oat01-original" {
		t.Fatalf("observeToken = %q, want inbound bearer surfaced on rotator error", got)
	}
	if didSwap {
		t.Fatalf("didSwap = true, want false on rotator error")
	}
	if outbound := req.Header.Get("Authorization"); outbound != "Bearer sk-ant-oat01-original" {
		t.Fatalf("outbound Authorization = %q, want original preserved", outbound)
	}
	if sink.tokenCalls != 1 {
		t.Fatalf("Token calls = %d, want 1 (the failed attempt)", sink.tokenCalls)
	}
}

// TestSwapAuthorizationSkipsEnvironmentPaths covers the path allowlist:
// /v1/environments/<env_id>/* is resource-bound on the upstream, so we
// pass the original bearer through to keep Anthropic's env-to-bearer
// binding intact. The observe path still gets the inbound bearer so
// rotator quota state stays current.
func TestSwapAuthorizationSkipsEnvironmentPaths(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{token: "fresh-token"}
	req := httptest.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/environments/env_01ABC/work/poll?ack=true", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-original")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	if got != "sk-ant-oat01-original" {
		t.Fatalf("observeToken = %q, want inbound bearer surfaced for /v1/environments/*", got)
	}
	if didSwap {
		t.Fatalf("didSwap = true, want false for /v1/environments/*")
	}
	if outbound := req.Header.Get("Authorization"); outbound != "Bearer sk-ant-oat01-original" {
		t.Fatalf("outbound Authorization = %q, want passthrough", outbound)
	}
	if sink.tokenCalls != 0 {
		t.Fatalf("Token calls = %d, want 0 for non-allowlisted path", sink.tokenCalls)
	}
}

// TestSwapAuthorizationSkipsSessionPaths covers the same allowlist gate
// for session-bound resources. Same rationale as environment paths:
// session_* IDs are bound to the creating bearer upstream.
func TestSwapAuthorizationSkipsSessionPaths(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{token: "fresh-token"}
	req := httptest.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/sessions/session_01ABC/watch", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-original")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	if got != "sk-ant-oat01-original" {
		t.Fatalf("observeToken = %q, want inbound bearer surfaced for /v1/sessions/*", got)
	}
	if didSwap {
		t.Fatalf("didSwap = true, want false for /v1/sessions/*")
	}
	if sink.tokenCalls != 0 {
		t.Fatalf("Token calls = %d, want 0 for non-allowlisted path", sink.tokenCalls)
	}
}

// TestSwapAuthorizationAllowsCountTokens confirms the allowlist prefix
// match covers /v1/messages/count_tokens too, not just exact /v1/messages.
func TestSwapAuthorizationAllowsCountTokens(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{token: "fresh-token"}
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-original")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	if !didSwap {
		t.Fatalf("didSwap = false, want true for /v1/messages/count_tokens")
	}
	if got != "fresh-token" {
		t.Fatalf("observeToken = %q, want fresh-token", got)
	}
}

// TestSwapAuthorizationAllowsModels confirms /v1/models is on the swap
// allowlist alongside /v1/messages.
func TestSwapAuthorizationAllowsModels(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{token: "fresh-token"}
	req := httptest.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-ant-oat01-original")

	got, didSwap := swapAuthorizationForRotator(req, sink, discardLogger())

	if !didSwap {
		t.Fatalf("didSwap = false, want true for /v1/models")
	}
	if got != "fresh-token" {
		t.Fatalf("observeToken = %q, want fresh-token", got)
	}
}

func TestObserveAnthropicResponseEmitsSignal(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-Status":               []string{"allowed"},
			"Anthropic-Ratelimit-Unified-Reset":                []string{"1700000000"},
			"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"five_hour"},
		},
	}

	observeAnthropicResponse(context.Background(), sink, resp, "fresh-token", discardLogger())

	signals := sink.snapshot()
	if len(signals) != 1 {
		t.Fatalf("Observe calls = %d, want 1", len(signals))
	}
	sig := signals[0]
	if sig.AccessToken != "fresh-token" {
		t.Fatalf("signal access token = %q, want fresh-token", sig.AccessToken)
	}
	if sig.Status != ratelimitsink.StatusAllowed {
		t.Fatalf("signal status = %q, want allowed", sig.Status)
	}
	if sig.Claim != ratelimitsink.ClaimFiveHour {
		t.Fatalf("signal claim = %q, want five_hour", sig.Claim)
	}
	if sig.HTTPStatus != http.StatusOK {
		t.Fatalf("signal http status = %d, want 200", sig.HTTPStatus)
	}
	if sig.ResetAt.Unix() != 1700000000 {
		t.Fatalf("signal reset = %d, want 1700000000", sig.ResetAt.Unix())
	}
	if sig.ObservedAt.IsZero() {
		t.Fatalf("signal observed_at must be populated")
	}
}

func TestObserveAnthropicResponseSkipsWhenNotSwapped(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Anthropic-Ratelimit-Unified-Status": []string{"allowed"}},
	}

	observeAnthropicResponse(context.Background(), sink, resp, "", discardLogger())

	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("Observe calls = %d, want 0 when swappedToken is empty", len(got))
	}
}

func TestObserveAnthropicResponseSkipsWhenSinkIsNil(t *testing.T) {
	t.Parallel()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	// No panic: this is the safety net for the passthrough wiring.
	observeAnthropicResponse(context.Background(), nil, resp, "fresh-token", discardLogger())
}

func TestObserveAnthropicResponseMapsRejectedOn429(t *testing.T) {
	t.Parallel()
	sink := &stubRotatorSink{}
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Anthropic-Ratelimit-Unified-Representative-Claim": []string{"seven_day"},
		},
	}

	observeAnthropicResponse(context.Background(), sink, resp, "fresh-token", discardLogger())

	signals := sink.snapshot()
	if len(signals) != 1 {
		t.Fatalf("Observe calls = %d, want 1", len(signals))
	}
	if signals[0].Status != ratelimitsink.StatusRejected {
		t.Fatalf("status = %q, want rejected on 429", signals[0].Status)
	}
	if signals[0].Claim != ratelimitsink.ClaimSevenDay {
		t.Fatalf("claim = %q, want seven_day", signals[0].Claim)
	}
}

// errStubBoom is the canned error stubRotatorSink returns when the
// test wants to exercise the rotator-error branch of the swap helper.
// It is not wrapped because the helper's behavior depends only on
// (err != nil), not on the wrapped chain.
var errStubBoom = stubError("stub: token unavailable")

type stubError string

func (e stubError) Error() string { return string(e) }
