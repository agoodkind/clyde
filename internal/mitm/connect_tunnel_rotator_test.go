package mitm

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// connectTunnelRotatorFixture stands up a TLS-intercepted CONNECT path
// the same way TestHandleConnectInterceptsCursorTLSAndCapturesRawFiles
// does, but with a stub rotator wired into the proxy so the test can
// assert that the bearer the upstream receives is the rotator-owned
// one and that Observe fires with the response shape.
type connectTunnelRotatorFixture struct {
	host  string
	proxy *testProxy

	mu             sync.Mutex
	receivedAuths  []string
	upstreamStatus int
	statusSequence []int
	upstreamHits   int
}

func newConnectTunnelRotatorFixture(t *testing.T) *connectTunnelRotatorFixture {
	t.Helper()
	const host = "api.anthropic.com"
	fixture := &connectTunnelRotatorFixture{
		host:           host,
		upstreamStatus: http.StatusOK,
	}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.Close)
	fixture.proxy = startCursorMITMTestProxy(t, t.TempDir(), host, upstream, true)
	t.Cleanup(fixture.proxy.shutdown)
	return fixture
}

func (f *connectTunnelRotatorFixture) authHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.receivedAuths))
	copy(out, f.receivedAuths)
	return out
}

func (f *connectTunnelRotatorFixture) hitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upstreamHits
}

// roundTripThroughCONNECT issues a single CONNECT + intercepted-TLS
// HTTPS request through the proxy and returns the response body.
func (f *connectTunnelRotatorFixture) roundTripThroughCONNECT(t *testing.T, authHeader string) *http.Response {
	t.Helper()
	caPool := x509.NewCertPool()
	caPool.AddCert(f.proxy.proxy.ca.cert)
	client, err := net.DialTimeout("tcp", f.proxy.addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := fmt.Fprintf(client, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", f.host, f.host); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(client)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		t.Fatalf("CONNECT status = %q", strings.TrimSpace(statusLine))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	tlsClient := tls.Client(&bufferedConn{Conn: client, reader: br}, &tls.Config{
		ServerName: f.host,
		RootCAs:    caPool,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	t.Cleanup(func() { _ = tlsClient.Close() })

	req, err := http.NewRequest(http.MethodPost, "https://"+f.host+"/v1/messages", strings.NewReader(`{"model":"claude"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if err := req.Write(tlsClient); err != nil {
		t.Fatalf("write TLS HTTP request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsClient), req)
	if err != nil {
		t.Fatalf("read TLS HTTP response: %v", err)
	}
	return resp
}

func TestConnectTunnelSwapsAndObservesAnthropicBearer(t *testing.T) {
	fixture := newConnectTunnelRotatorFixture(t)
	sink := &stubRotatorSink{token: "fresh-token", account: "acct-1"}
	fixture.proxy.proxy.SetRotatorSink(sink)

	resp := fixture.roundTripThroughCONNECT(t, "Bearer sk-ant-oat01-inbound")
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	auths := fixture.authHeaders()
	if len(auths) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(auths))
	}
	if auths[0] != "Bearer fresh-token" {
		t.Fatalf("upstream Authorization = %q, want Bearer fresh-token", auths[0])
	}
	signals := sink.snapshot()
	if len(signals) != 1 {
		t.Fatalf("observe count = %d, want 1", len(signals))
	}
	if signals[0].AccessToken != "fresh-token" {
		t.Fatalf("observed token = %q, want fresh-token", signals[0].AccessToken)
	}
}

func TestConnectTunnelRetriesOn401(t *testing.T) {
	fixture := newConnectTunnelRotatorFixture(t)
	fixture.statusSequence = []int{http.StatusUnauthorized, http.StatusOK}
	sink := &stubRotatorSink{token: "stale-token", afterFailure: "after-failure-token"}
	fixture.proxy.proxy.SetRotatorSink(sink)

	resp := fixture.roundTripThroughCONNECT(t, "Bearer sk-ant-oat01-inbound")
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after retry", resp.StatusCode)
	}
	if hits := fixture.hitCount(); hits != 2 {
		t.Fatalf("upstream hits = %d, want 2 (one 401 + one retry)", hits)
	}
	auths := fixture.authHeaders()
	if auths[0] != "Bearer stale-token" {
		t.Fatalf("first auth = %q, want Bearer stale-token", auths[0])
	}
	if auths[1] != "Bearer after-failure-token" {
		t.Fatalf("retry auth = %q, want Bearer after-failure-token", auths[1])
	}
	if sink.afterFailureCalls != 1 {
		t.Fatalf("TokenAfterAuthFailure calls = %d, want 1", sink.afterFailureCalls)
	}
	signals := sink.snapshot()
	if len(signals) != 1 {
		t.Fatalf("observe count = %d, want 1 (final response only)", len(signals))
	}
	if signals[0].AccessToken != "after-failure-token" {
		t.Fatalf("observed token = %q, want after-failure-token", signals[0].AccessToken)
	}
}

func TestConnectTunnelSurfaces401WhenTokenUnchanged(t *testing.T) {
	fixture := newConnectTunnelRotatorFixture(t)
	fixture.upstreamStatus = http.StatusUnauthorized
	sink := &stubRotatorSink{token: "stale-token", afterFailureErr: errStubTokenUnchanged}
	fixture.proxy.proxy.SetRotatorSink(sink)

	resp := fixture.roundTripThroughCONNECT(t, "Bearer sk-ant-oat01-inbound")
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no retry on unchanged)", resp.StatusCode)
	}
	if hits := fixture.hitCount(); hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no retry)", hits)
	}
}

func TestConnectTunnelPassthroughWhenSinkNil(t *testing.T) {
	fixture := newConnectTunnelRotatorFixture(t)
	// No SetRotatorSink: passthrough.

	resp := fixture.roundTripThroughCONNECT(t, "Bearer sk-ant-oat01-inbound")
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	auths := fixture.authHeaders()
	if len(auths) != 1 {
		t.Fatalf("upstream hits = %d, want 1", len(auths))
	}
	if auths[0] != "Bearer sk-ant-oat01-inbound" {
		t.Fatalf("upstream auth = %q, want passthrough Bearer sk-ant-oat01-inbound", auths[0])
	}
}
