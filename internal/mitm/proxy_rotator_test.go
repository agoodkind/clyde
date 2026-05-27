package mitm

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/slogger"
)

// errStubTokenUnchanged is the test analogue of
// oauthrotation.ErrTokenUnchanged so the proxy_test can exercise the
// "401 unchanged-token, no retry" branch without importing the rotator
// package directly. Production behavior treats any non-nil error from
// TokenAfterAuthFailure the same way (no retry), so the exact sentinel
// value does not need to match the rotator's; what matters is that the
// stub returns a non-nil error and the proxy honors it.
var errStubTokenUnchanged = errors.New("stub: token unchanged after auth failure")

// rotatorProxyFixture wires a *Proxy in front of an httptest.Server so
// the proxy's dispatchUpstream path runs against a real upstream and
// the test can assert on the headers the upstream observed.
type rotatorProxyFixture struct {
	proxy    *Proxy
	upstream *httptest.Server

	mu             sync.Mutex
	receivedAuths  []string
	upstreamHits   int
	upstreamStatus int
	statusSequence []int
}

func newRotatorProxyFixture(t *testing.T) *rotatorProxyFixture {
	t.Helper()
	fixture := &rotatorProxyFixture{
		upstreamStatus: http.StatusOK,
	}
	fixture.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.upstreamHits++
		fixture.receivedAuths = append(fixture.receivedAuths, r.Header.Get("Authorization"))
		var status int
		if fixture.upstreamHits-1 < len(fixture.statusSequence) {
			status = fixture.statusSequence[fixture.upstreamHits-1]
		} else {
			status = fixture.upstreamStatus
		}
		fixture.mu.Unlock()
		w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(fixture.upstream.Close)
	t.Cleanup(registerTestRoute(t, testRouteOpenAIV1, fixture.upstream.URL))
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fixture.proxy = &Proxy{
		log:             logger,
		client:          fixture.upstream.Client(),
		dialContext:     (&net.Dialer{}).DialContext,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		rawCaptureSeq:   atomic.Uint64{},
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		rotatorMu:       sync.RWMutex{},
		rotatorSink:     nil,
		Tunnels:         newTestTunnelRegistry(),
		captureWriters:  newCaptureWriterCache(logger),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: t.TempDir()},
		base:            "http://[::1]",
		server:          nil,
	}
	t.Cleanup(fixture.proxy.closeCaptureWriters)
	return fixture
}

func (f *rotatorProxyFixture) lastAuthHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.receivedAuths) == 0 {
		return ""
	}
	return f.receivedAuths[len(f.receivedAuths)-1]
}

func (f *rotatorProxyFixture) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.receivedAuths))
	copy(out, f.receivedAuths)
	return out
}

func (f *rotatorProxyFixture) hitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upstreamHits
}

func sendProxyRequest(t *testing.T, proxy *Proxy, authHeader string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/messages", strings.NewReader(`{"model":"claude"}`))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	proxy.handle(recorder, req)
	return recorder.Result()
}

func TestProxyDispatchUpstreamSwapsAndObserves(t *testing.T) {
	fixture := newRotatorProxyFixture(t)
	sink := &stubRotatorSink{token: "fresh-token", account: "acct-1"}
	fixture.proxy.SetRotatorSink(sink)

	resp := sendProxyRequest(t, fixture.proxy, "Bearer sk-ant-oat01-inbound")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d, want 200", resp.StatusCode)
	}
	if got := fixture.lastAuthHeader(); got != "Bearer fresh-token" {
		t.Fatalf("upstream Authorization = %q, want Bearer fresh-token", got)
	}
	signals := sink.snapshot()
	if len(signals) != 1 {
		t.Fatalf("observe count = %d, want 1", len(signals))
	}
	if signals[0].AccessToken != "fresh-token" {
		t.Fatalf("observed token = %q, want fresh-token", signals[0].AccessToken)
	}
	if signals[0].HTTPStatus != http.StatusOK {
		t.Fatalf("observed status = %d, want 200", signals[0].HTTPStatus)
	}
}

func TestProxyDispatchUpstreamRetriesOn401(t *testing.T) {
	fixture := newRotatorProxyFixture(t)
	fixture.statusSequence = []int{http.StatusUnauthorized, http.StatusOK}
	sink := &stubRotatorSink{token: "stale-token", afterFailure: "after-failure-token"}
	fixture.proxy.SetRotatorSink(sink)

	resp := sendProxyRequest(t, fixture.proxy, "Bearer sk-ant-oat01-inbound")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d, want 200 after retry; body=%s", resp.StatusCode, body)
	}
	if hits := fixture.hitCount(); hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (one 401 + one retry)", hits)
	}
	auths := fixture.authHeaders()
	if len(auths) < 2 {
		t.Fatalf("auth headers = %#v, want at least 2", auths)
	}
	if auths[0] != "Bearer stale-token" {
		t.Fatalf("first request Authorization = %q, want Bearer stale-token", auths[0])
	}
	if auths[1] != "Bearer after-failure-token" {
		t.Fatalf("retry request Authorization = %q, want Bearer after-failure-token", auths[1])
	}
	if sink.afterFailureCalls != 1 {
		t.Fatalf("TokenAfterAuthFailure calls = %d, want 1", sink.afterFailureCalls)
	}
	if sink.lastFailedToken != "stale-token" {
		t.Fatalf("TokenAfterAuthFailure failedToken = %q, want stale-token", sink.lastFailedToken)
	}
	signals := sink.snapshot()
	if len(signals) != 1 {
		t.Fatalf("observe count = %d, want 1 (only the final response is observed)", len(signals))
	}
	if signals[0].AccessToken != "after-failure-token" {
		t.Fatalf("observed token = %q, want after-failure-token", signals[0].AccessToken)
	}
	if signals[0].HTTPStatus != http.StatusOK {
		t.Fatalf("observed status = %d, want 200 (the retry response)", signals[0].HTTPStatus)
	}
}

func TestProxyDispatchUpstreamSurfaces401WhenTokenUnchanged(t *testing.T) {
	fixture := newRotatorProxyFixture(t)
	fixture.upstreamStatus = http.StatusUnauthorized
	sink := &stubRotatorSink{token: "stale-token", afterFailureErr: errStubTokenUnchanged}
	fixture.proxy.SetRotatorSink(sink)

	resp := sendProxyRequest(t, fixture.proxy, "Bearer sk-ant-oat01-inbound")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response status = %d, want 401 (no retry on unchanged)", resp.StatusCode)
	}
	if hits := fixture.hitCount(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no retry)", hits)
	}
	if sink.afterFailureCalls != 1 {
		t.Fatalf("TokenAfterAuthFailure calls = %d, want 1", sink.afterFailureCalls)
	}
}

func TestProxyDispatchUpstreamPassthroughWhenSinkNil(t *testing.T) {
	fixture := newRotatorProxyFixture(t)
	// SetRotatorSink intentionally NOT called: passthrough mode.

	resp := sendProxyRequest(t, fixture.proxy, "Bearer sk-ant-oat01-inbound")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := fixture.lastAuthHeader(); got != "Bearer sk-ant-oat01-inbound" {
		t.Fatalf("upstream Authorization = %q, want passthrough", got)
	}
}

func TestProxyDispatchUpstreamSkipsObserveOnNonAnthropicBearer(t *testing.T) {
	fixture := newRotatorProxyFixture(t)
	sink := &stubRotatorSink{token: "fresh-token"}
	fixture.proxy.SetRotatorSink(sink)

	resp := sendProxyRequest(t, fixture.proxy, "Bearer not-an-anthropic-token")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := fixture.lastAuthHeader(); got != "Bearer not-an-anthropic-token" {
		t.Fatalf("upstream Authorization = %q, want unchanged", got)
	}
	if len(sink.snapshot()) != 0 {
		t.Fatalf("observe count = %d, want 0 (non-anthropic bearer was not swapped)", len(sink.snapshot()))
	}
}

// sendProxyRequestPath issues a request through the proxy targeting an
// arbitrary path. The test fixture's upstream accepts any path; this
// helper exists so the test can drive paths outside the bearer-swap
// allowlist (e.g. /v1/environments/*) through the same dispatchUpstream
// codepath as the messages-targeted tests above.
func sendProxyRequestPath(t *testing.T, proxy *Proxy, path, authHeader string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://clyde.test"+path, http.NoBody)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	recorder := httptest.NewRecorder()
	proxy.handle(recorder, req)
	return recorder.Result()
}

// TestProxyDispatchUpstreamPassesEnvironmentPathThrough exercises the
// path allowlist gate end-to-end. A /v1/environments/<env_id>/work/poll
// poll carries an Anthropic OAuth bearer, but the upstream binds the env
// to the creating bearer; swapping it would 403. The proxy must leave
// the Authorization header untouched and still emit one observe signal
// against the inbound bearer so the rotator sees the response's
// rate-limit headers.
func TestProxyDispatchUpstreamPassesEnvironmentPathThrough(t *testing.T) {
	fixture := newRotatorProxyFixture(t)
	sink := &stubRotatorSink{token: "fresh-token", account: "acct-1"}
	fixture.proxy.SetRotatorSink(sink)

	resp := sendProxyRequestPath(t, fixture.proxy, "/v1/environments/env_01ABC/work/poll?ack=true", "Bearer sk-ant-oat01-inbound")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if got := fixture.lastAuthHeader(); got != "Bearer sk-ant-oat01-inbound" {
		t.Fatalf("upstream Authorization = %q, want passthrough (no swap on /v1/environments/*)", got)
	}
	if sink.tokenCalls != 0 {
		t.Fatalf("Token calls = %d, want 0 for non-allowlisted path", sink.tokenCalls)
	}
	signals := sink.snapshot()
	if len(signals) != 1 {
		t.Fatalf("observe count = %d, want 1 (observe still fires using inbound bearer)", len(signals))
	}
	if signals[0].AccessToken != "sk-ant-oat01-inbound" {
		t.Fatalf("observed token = %q, want inbound bearer surfaced to Observe", signals[0].AccessToken)
	}
}

// TestProxyDispatchUpstreamDoesNotRetryOn401WhenPathNotSwapped covers
// the other half of the path-scoping contract: when the upstream
// returns 401 on a passthrough request, the proxy must NOT invoke
// TokenAfterAuthFailure. Retrying with a rotator-owned bearer on a path
// we deliberately routed around would put a different token on a
// resource-bound endpoint and reintroduce the 403 cascade the gate
// exists to prevent.
func TestProxyDispatchUpstreamDoesNotRetryOn401WhenPathNotSwapped(t *testing.T) {
	fixture := newRotatorProxyFixture(t)
	fixture.upstreamStatus = http.StatusUnauthorized
	sink := &stubRotatorSink{token: "fresh-token", afterFailure: "after-failure-token"}
	fixture.proxy.SetRotatorSink(sink)

	resp := sendProxyRequestPath(t, fixture.proxy, "/v1/environments/env_01ABC/work/poll", "Bearer sk-ant-oat01-inbound")
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("response status = %d, want 401 propagated unchanged", resp.StatusCode)
	}
	if hits := fixture.hitCount(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no retry on passthrough path)", hits)
	}
	if sink.afterFailureCalls != 0 {
		t.Fatalf("TokenAfterAuthFailure calls = %d, want 0 on passthrough path", sink.afterFailureCalls)
	}
}
