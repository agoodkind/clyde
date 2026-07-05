package mitm

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm/capture"
)

// TestHandleConnectSplicesUnclaimedServerFirstWithoutCapture covers the
// non-TLS branch of the unclaimed CONNECT sniff: the client stays silent and a
// raw upstream speaks first. The proxy must opaque-splice (not TLS-intercept)
// so the upstream's leading bytes reach the client byte-for-byte, and an opaque
// splice never writes a capture row. The keepalive and drain tests exercise the
// same server-speaks-first path; this test pins the routing and the deliberate
// no-capture behavior on that branch.
func TestHandleConnectSplicesUnclaimedServerFirstWithoutCapture(t *testing.T) {
	payload := []byte("SERVER-FIRST-NON-TLS-STREAM-PAYLOAD-DO-NOT-CAPTURE")
	upstreamAddr, stopUpstream := startSlowDripUpstream(t, payload, time.Millisecond)
	defer stopUpstream()

	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	caDir := t.TempDir()
	captureDir := t.TempDir()
	dbPath := filepath.Join(captureDir, "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	storeOpen := true
	t.Cleanup(func() {
		if storeOpen {
			_ = store.Close(context.Background(), "test cleanup")
		}
	})
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "ca.crt"),
			KeyPath:  filepath.Join(caDir, "ca.key"),
		},
		CaptureDir: captureDir,
	}
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	proxy, err := NewProxy(mitmCfg, config.LoggingRequest{}, nil, []net.Listener{listener}, nil, store, "test", group)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = proxy.Serve(context.Background())
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = proxy.ShutdownHTTP(ctx)
		<-serveDone
	})

	tunnel := openConnectTunnel(t, proxy.base, upstreamAddr)
	if err := tunnel.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set tunnel deadline: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(tunnel, got); err != nil {
		t.Fatalf("read spliced payload: %v", err)
	}
	_ = tunnel.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("spliced payload = %q want %q", got, payload)
	}

	// An opaque splice never records the exchange, so the store stays empty.
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeOpen = false
	assertNoCaptureRows(t, dbPath)
}

// TestStopPendingConnectReadClearsDeadline guards the sniffer's deadline
// hygiene: stopPendingConnectRead expires the read deadline to interrupt a
// pending byte read, but it must clear the deadline afterward so the connection
// it hands to the splice or the terminated TLS handshake is not left with a
// stale past deadline that instantly times out every later read.
func TestStopPendingConnectReadClearsDeadline(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()

	// Seed the interrupted state: a past deadline, as the pending read leaves it.
	if err := local.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("seed deadline: %v", err)
	}
	readCh := make(chan connectByteRead, 1)
	readCh <- connectByteRead{value: transparentTLSHandshakeRecord, ok: true}

	got := stopPendingConnectRead(local, readCh)
	if !got.ok || got.value != transparentTLSHandshakeRecord {
		t.Fatalf("stopPendingConnectRead = %+v, want the buffered byte", got)
	}

	// With the deadline cleared, a read with no data blocks. Without the clear it
	// returns an i/o timeout immediately.
	done := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := local.Read(b[:])
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("read returned early (%v); stopPendingConnectRead left a stale deadline", err)
	case <-time.After(150 * time.Millisecond):
		// Blocked as expected: the deadline was cleared.
	}
}

func assertNoCaptureRows(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&count); err != nil {
		t.Fatalf("count capture rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("capture rows = %d, want 0 (opaque splice must not capture)", count)
	}
}
