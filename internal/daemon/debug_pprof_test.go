package daemon

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

func TestPprofListenWhenEnabled(t *testing.T) {
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	startDebugPprofListener(log, config.PprofConfig{Enabled: true, Listen: "localhost:0"})

	addr := waitForPprofAddr(t, logBuf, 2*time.Second)
	resp, err := httpGetWithTimeout("http://"+addr+"/debug/pprof/goroutine?debug=1", 2*time.Second)
	if err != nil {
		t.Fatalf("GET /debug/pprof/goroutine: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from pprof, got %d", resp.StatusCode)
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("read pprof body: %v", readErr)
	}
	if len(body) == 0 {
		t.Errorf("expected non-empty pprof body, got %d bytes", len(body))
	}
}

func TestPprofSkippedWhenDisabled(t *testing.T) {
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	preProbeListener, listenErr := net.Listen("tcp", "localhost:0")
	if listenErr != nil {
		t.Fatalf("probe listen: %v", listenErr)
	}
	probeAddr := preProbeListener.Addr().String()
	if closeErr := preProbeListener.Close(); closeErr != nil {
		t.Fatalf("close probe listener: %v", closeErr)
	}

	startDebugPprofListener(log, config.PprofConfig{Enabled: false, Listen: probeAddr})

	if strings.Contains(logBuf.String(), "debug.pprof.listening") {
		t.Errorf("expected no listening event when disabled, got log %q", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "debug.pprof.listen_failed") {
		t.Errorf("expected no listen_failed event when disabled, got log %q", logBuf.String())
	}
}

// waitForPprofAddr scans the test log buffer for the debug.pprof.listening
// event and returns the addr attribute it carries. The deadline keeps the
// test from blocking forever if the listener never comes up.
func waitForPprofAddr(t *testing.T, logBuf *bytes.Buffer, deadline time.Duration) string {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		snapshot := logBuf.String()
		index := strings.Index(snapshot, "\"debug.pprof.listening\"")
		if index < 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		marker := "\"addr\":\""
		fragment := snapshot[index:]
		addrIndex := strings.Index(fragment, marker)
		if addrIndex < 0 {
			t.Fatalf("listening event missing addr attribute: %q", fragment)
		}
		addrStart := addrIndex + len(marker)
		end := strings.Index(fragment[addrStart:], "\"")
		if end < 0 {
			t.Fatalf("listening event addr attribute unterminated: %q", fragment)
		}
		return fragment[addrStart : addrStart+end]
	}
	t.Fatalf("pprof listener did not announce within %s; log=%q", deadline, logBuf.String())
	return ""
}

// httpGetWithTimeout issues a GET with a hard timeout so the test never
// blocks on a misbehaving server.
func httpGetWithTimeout(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	return client.Get(url)
}
