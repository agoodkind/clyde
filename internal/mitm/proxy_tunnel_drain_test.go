package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// TestProxyShutdownDrainsCloudflareKeepaliveTunnel simulates the
// Cloudflare keepalive case: a CONNECT tunnel whose upstream never
// closes its read side. Without livetrack, http.Server.Shutdown
// would block the full ctx deadline and tunnel goroutines would
// keep running. With livetrack, the registry's force-close fan-out
// terminates both ends within the configured grace and the registry
// count returns to zero.
func TestProxyShutdownDrainsCloudflareKeepaliveTunnel(t *testing.T) {
	upstream, upstreamAddr, stopUpstream := startKeepaliveUpstream(t)
	defer stopUpstream()
	_ = upstream

	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	caDir := t.TempDir()
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "ca.crt"),
			KeyPath:  filepath.Join(caDir, "ca.key"),
		},
		CaptureDir: t.TempDir(),
	}
	proxy, err := NewProxy(mitmCfg, config.LoggingRequest{}, nil, []net.Listener{listener}, nil, "test", livetrack.NewGroup(livetrack.GroupOptions{Log: nil}))
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = proxy.Serve()
	}()

	connectClient(t, proxy.base, upstreamAddr)
	waitForCount(t, proxy, 1, 2*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- proxy.Shutdown(ctx)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("Shutdown did not return within 5s; tunnel goroutines pinned reload")
	}
	if got := proxy.Tunnels.Count(); got != 0 {
		t.Fatalf("Tunnels.Count after shutdown: got %d, want 0", got)
	}
	<-serveDone
}

// TestProxyShutdownPreservesInFlightTunnelUntilUpstreamCloses is the
// CLYDE-324 regression: an HTTPS tunnel actively forwarding response
// bytes when reload fires must not be force-closed mid-stream. The
// proxy's Shutdown is invoked with a long-lived context that mirrors
// the daemon's mitmReloadDrainCap; the test then proves the client
// receives every byte the upstream writes before EOF.
//
// The fake upstream drips a short payload over multiple chunks with a
// 60ms cadence so the response straddles the wall-clock window
// reload would have force-closed under the prior 4s deadline. Reading
// the full payload back and comparing sha256 confirms there is no
// truncation, no corruption, and no extra bytes.
func TestProxyShutdownPreservesInFlightTunnelUntilUpstreamCloses(t *testing.T) {
	// Payload sized so the slow drip straddles the prior 4s force-close
	// deadline (CLYDE-324 root cause) without making the test too slow.
	// 100 chunks of 16 bytes at 60ms cadence = ~6s wall clock, ~1600
	// bytes total. Anything that crosses the 4s threshold proves the
	// fix; a longer drip would only slow the test.
	payload := bytes.Repeat([]byte("clyde-324-"), 160)
	wantHash := sha256.Sum256(payload)

	upstreamAddr, stopUpstream := startSlowDripUpstream(t, payload, 60*time.Millisecond)
	defer stopUpstream()

	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	caDir := t.TempDir()
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "ca.crt"),
			KeyPath:  filepath.Join(caDir, "ca.key"),
		},
		CaptureDir: t.TempDir(),
	}
	proxy, err := NewProxy(mitmCfg, config.LoggingRequest{}, nil, []net.Listener{listener}, nil, "test", livetrack.NewGroup(livetrack.GroupOptions{Log: nil}))
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = proxy.Serve()
	}()

	clientConn := openConnectTunnel(t, proxy.base, upstreamAddr)
	defer func() { _ = clientConn.Close() }()
	waitForCount(t, proxy, 1, 2*time.Second)

	readDone := make(chan readResult, 1)
	go func() {
		buf := bytes.NewBuffer(nil)
		_, copyErr := io.Copy(buf, clientConn)
		readDone <- readResult{bytes: buf.Bytes(), err: copyErr}
	}()

	// Trigger Shutdown while the upstream is still mid-stream. The
	// long ctx mirrors the daemon's drain cap so the proxy waits for
	// the in-flight tunnel to drain naturally rather than force-
	// closing under a short deadline.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- proxy.Shutdown(shutdownCtx)
	}()

	select {
	case res := <-readDone:
		if res.err != nil && !errors.Is(res.err, io.EOF) {
			t.Fatalf("client read failed: %v", res.err)
		}
		if len(res.bytes) != len(payload) {
			t.Fatalf("client received %d bytes, want %d (truncation under reload force-close?)", len(res.bytes), len(payload))
		}
		gotHash := sha256.Sum256(res.bytes)
		if gotHash != wantHash {
			t.Fatalf("client received corrupted payload: got sha256=%x, want %x", gotHash, wantHash)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("client did not receive full payload before timeout")
	}

	// Real HTTPS clients (Claude CLI, Cursor, Codex) close the TCP
	// session after the response completes (or after a TLS shutdown
	// alert). The test mirrors that here so the proxy's splice
	// goroutines unwind via EOF on the up-direction read and the
	// tunnel registers Release naturally rather than via force-close.
	_ = clientConn.Close()

	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("proxy.Shutdown returned err: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("proxy.Shutdown did not return after tunnel completed")
	}
	if got := proxy.Tunnels.Count(); got != 0 {
		t.Fatalf("Tunnels.Count after shutdown: got %d, want 0", got)
	}
	<-serveDone
}

// TestRegisterPlainHTTPDecouplesUpstreamFromInboundContext is the
// CLYDE-324 fix #3 regression: the upstream context returned by
// registerPlainHTTP must not be cancelled when the inbound request's
// context is cancelled. The bug was that registerPlainHTTP derived
// its tunnel context from r.Context() and mutated r to carry it, so
// the inbound context's lifecycle (which the stdlib http.Server
// cancels at unrelated state transitions) silently aborted in-flight
// upstream HTTPS streams. The fix derives the tunnel context from
// p.ctx (the proxy-lifetime context) instead.
//
// This test would fail on the pre-fix code: the cancelled inbound
// context would propagate to the returned upstream context within
// microseconds. After the fix, the upstream context survives until
// the registered closer fires (force-close) or release runs (handler
// return).
func TestRegisterPlainHTTPDecouplesUpstreamFromInboundContext(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := &Proxy{
		log:             logger,
		httpClient:      http.DefaultClient,
		dialContext:     nil,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		Tunnels:         newTestTunnelRegistry(),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{},
		base:            "http://[::1]",
		server:          nil,
	}

	// Build a request whose context we can cancel independently of the
	// proxy lifetime. Pre-fix, registerPlainHTTP derived its context
	// from r.Context() (and mutated r), so cancelling parentCancel
	// below would cancel the returned upstream context too.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	t.Cleanup(parentCancel)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)).WithContext(parentCtx)
	rec := httptest.NewRecorder()

	// Mirror the call shape from handle: derive upstreamCtx from
	// r.Context() via WithoutCancel + WithCancel (the CLYDE-324
	// pattern), then register the tunnel.
	upstreamCtx, upstreamCancel := context.WithCancel(context.WithoutCancel(req.Context()))
	t.Cleanup(upstreamCancel)
	_, release, ok := proxy.registerPlainHTTP(upstreamCtx, upstreamCancel, rec, req, "https://api.anthropic.com")
	if !ok {
		t.Fatalf("registerPlainHTTP rejected the request: status=%d body=%q", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { release("test.cleanup") })

	select {
	case <-upstreamCtx.Done():
		t.Fatalf("upstream context already cancelled before parent cancel; expected it to be live")
	default:
	}

	// Cancel the inbound request's context. Pre-fix (when ctx was
	// derived from r.Context() via plain WithCancel), this also
	// cancels upstreamCtx; that was the CLYDE-324 root cause. Post-fix
	// (with WithoutCancel breaking the chain), upstreamCtx survives.
	parentCancel()

	// Give the parent cancellation a generous moment to propagate. If
	// the contexts were tied, upstreamCtx.Done() would fire essentially
	// instantly. Polling for 100ms catches any tied propagation while
	// staying fast enough to keep the test cheap.
	select {
	case <-upstreamCtx.Done():
		t.Fatalf("upstream context cancelled by inbound context: contexts are tied (CLYDE-324 regression). err=%v", upstreamCtx.Err())
	case <-time.After(100 * time.Millisecond):
		// Contexts are correctly decoupled.
	}

	// Sanity: the upstream context must still cancel via its own
	// closer (this is the legitimate force-close path the registry
	// drives during reload).
	proxy.Tunnels.ForceCloseMatching(context.Background(), func(_ livetrack.Session[TunnelMeta]) bool { return true }, "test.force_close")
	select {
	case <-upstreamCtx.Done():
		// Force-close path correctly fires upstream cancel.
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("upstream context did not cancel after force-close; the closer chain is broken")
	}
}

type readResult struct {
	bytes []byte
	err   error
}

// startSlowDripUpstream returns an addr to a TCP server that, on the
// first accepted connection, writes payload in 16-byte chunks at the
// supplied cadence and then half-closes the write side so the client
// observes a clean EOF. Subsequent connects are accepted and closed
// immediately so the listener stays drainable.
func startSlowDripUpstream(t *testing.T, payload []byte, chunkInterval time.Duration) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	stopFlag := atomic.Bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		first := true
		for {
			conn, acceptErr := lis.Accept()
			if acceptErr != nil {
				return
			}
			if !first {
				_ = conn.Close()
				continue
			}
			first = false
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				const chunkSize = 16
				for offset := 0; offset < len(payload); offset += chunkSize {
					if stopFlag.Load() {
						return
					}
					end := offset + chunkSize
					if end > len(payload) {
						end = len(payload)
					}
					if _, err := c.Write(payload[offset:end]); err != nil {
						return
					}
					time.Sleep(chunkInterval)
				}
				if cw, ok := c.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}(conn)
		}
	}()
	stop := func() {
		stopFlag.Store(true)
		_ = lis.Close()
		drainDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(drainDone)
		}()
		select {
		case <-drainDone:
		case <-time.After(2 * time.Second):
		}
	}
	return lis.Addr().String(), stop
}

// openConnectTunnel issues a CONNECT through the proxy to the
// supplied upstream and returns the established tunnel connection so
// the caller can read the upstream payload directly.
//
// The CONNECT response is parsed with a bufio.Reader, which can read
// past the response headers into the first bytes of the tunneled
// payload in a single syscall. Returning the bare conn would drop
// those buffered bytes and truncate the payload from the front (the
// CLYDE-324 drain test then reported a spurious tail-truncation). The
// returned conn drains the bufio buffer before the socket, mirroring
// how a real buffered client consumes the stream, so no delivered
// byte is lost.
func openConnectTunnel(t *testing.T, proxyBase string, upstreamAddr string) net.Conn {
	t.Helper()
	u, err := url.Parse(proxyBase)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	req := "CONNECT " + upstreamAddr + " HTTP/1.1\r\nHost: " + upstreamAddr + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		_ = conn.Close()
		t.Fatalf("set deadline: %v", err)
	}
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.Contains(statusLine, "200") {
		_ = conn.Close()
		t.Fatalf("CONNECT status: %q", statusLine)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		t.Fatalf("clear deadline: %v", err)
	}
	return &bufferedReadConn{Conn: conn, reader: reader}
}

// bufferedReadConn is a net.Conn whose Read drains a bufio.Reader
// before falling through to the underlying socket. It lets a helper
// that parsed a header preamble with bufio hand back a conn that still
// yields any payload bytes the reader buffered past the headers. All
// other conn methods (Write, Close, deadlines) operate on the embedded
// conn directly.
type bufferedReadConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedReadConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// connectClient issues a CONNECT through the proxy to the supplied
// upstream and immediately stops reading. The tunnel goroutines on
// the proxy side keep streaming bytes from the upstream's
// keepalive, simulating the Cloudflare hold-open.
func connectClient(t *testing.T, proxyBase string, upstreamAddr string) {
	t.Helper()
	u, err := url.Parse(proxyBase)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req := "CONNECT " + upstreamAddr + " HTTP/1.1\r\nHost: " + upstreamAddr + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	buf := make([]byte, 1024)
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
}

// waitForCount polls the registry until count >= want or the
// timeout elapses. Used so the test does not race the proxy's
// register goroutine.
func waitForCount(t *testing.T, proxy *Proxy, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if proxy.Tunnels.Count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Tunnels.Count: got %d, want >= %d after %s", proxy.Tunnels.Count(), want, timeout)
}

// startKeepaliveUpstream launches a TCP listener that accepts one
// connection and writes bytes forever on a 50ms cadence without
// reading or closing, simulating an upstream stuck behind a
// keepalive that never EOFs. Returns the addr and a stop func that
// closes the listener so leaked goroutines exit.
func startKeepaliveUpstream(t *testing.T) (*http.Server, string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	done := atomic.Bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				for {
					if done.Load() {
						return
					}
					<-ticker.C
					if _, err := c.Write([]byte(".")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	stop := func() {
		done.Store(true)
		_ = lis.Close()
		// Drain accept goroutine; the keepalive writers terminate
		// when their conn.Write fails on the closed listener.
		drainDone := make(chan struct{})
		go func() {
			wg.Wait()
			close(drainDone)
		}()
		select {
		case <-drainDone:
		case <-time.After(2 * time.Second):
		}
	}
	// Touch _ on http.Server to satisfy the helper signature without
	// pulling unused imports out of the test file.
	srv := &http.Server{Handler: http.NotFoundHandler()}
	_ = io.Discard
	return srv, lis.Addr().String(), stop
}
