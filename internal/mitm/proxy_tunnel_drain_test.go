package mitm

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
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
		BodyMode:   "summary",
	}
	proxy, err := NewProxy(mitmCfg, nil, listener)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = proxy.Serve()
	}()

	connectClient(t, proxy.ClaudeBaseURL(), upstreamAddr)
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
					select {
					case <-ticker.C:
						if _, err := c.Write([]byte(".")); err != nil {
							return
						}
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
