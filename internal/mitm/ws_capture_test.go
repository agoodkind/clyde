package mitm

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/slogger"
)

func TestProxyWebsocketCaptureRecordsFramesBothDirections(t *testing.T) {
	t.Parallel()

	// Spin up an upstream ws echo server.
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upstream upgrade: %v", err)
		}
		defer conn.Close()
		for {
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			reply := map[string]any{"echo": string(payload)}
			raw, _ := json.Marshal(reply)
			if err := conn.WriteMessage(mt, raw); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	dir := t.TempDir()
	p := newProxyForTest(t, config.MITMConfig{CaptureDir: dir})
	// Reroute the codex chatgpt upstream to the test server for the
	// duration of this test.
	defer overrideChatGPTUpstream(t, upstream.URL)()

	// Start the proxy on a random port.
	srv := httptest.NewServer(http.HandlerFunc(p.handle))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/backend-api/codex/responses"
	parsed, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parse ws url: %v", err)
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(context.Background(), parsed.String(), nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !strings.Contains(string(raw), `"echo"`) {
		t.Fatalf("expected echo, got %s", string(raw))
	}
	_ = conn.Close()

	// Drain capture file.
	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	for time.Now().Before(deadline) {
		f, err := os.Open(filepath.Join(dir, "capture.jsonl"))
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lines = lines[:0]
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		f.Close()
		if hasWSEvents(lines) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	phases := map[string]int{}
	directions := map[string]int{}
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse capture line: %v (%q)", err, line)
		}
		if ev["leg"] != string(logevent.LegMITMCaptureIndex) {
			continue
		}
		if phase, _ := ev["phase"].(string); phase != "" {
			phases[phase]++
		}
		mitmFields, ok := ev["mitm"].(map[string]any)
		if !ok {
			continue
		}
		if direction, _ := mitmFields["direction"].(string); direction != "" {
			directions[direction]++
		}
	}
	if phases[string(logevent.PhaseStarted)] < 1 {
		t.Errorf("expected websocket start record, got phases=%v directions=%v", phases, directions)
	}
	if directions["client_to_upstream"] < 1 {
		t.Errorf("expected client websocket message record, got phases=%v directions=%v", phases, directions)
	}
	if directions["upstream_to_client"] < 1 {
		t.Errorf("expected upstream websocket message record, got phases=%v directions=%v", phases, directions)
	}
	if phases[string(logevent.PhaseCompleted)] < 2 {
		t.Errorf("expected websocket completed records, got phases=%v directions=%v", phases, directions)
	}
}

func TestIsWebsocketUpgradeMatchesCaseInsensitive(t *testing.T) {
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("Upgrade", "WebSocket")
	r.Header.Set("Connection", "keep-alive, Upgrade")
	if !isWebsocketUpgrade(r) {
		t.Errorf("expected ws upgrade detection")
	}
}

func TestIsWebsocketUpgradeRejectsPlainHTTP(t *testing.T) {
	r := &http.Request{Header: http.Header{}}
	if isWebsocketUpgrade(r) {
		t.Errorf("plain HTTP should not detect ws")
	}
}

func newProxyForTest(t *testing.T, cfg config.MITMConfig) *Proxy {
	t.Helper()
	logger := slog.New(newMITMCaptureTestHandler(t, cfg.CaptureDir))
	proxy := &Proxy{
		log:                   logger,
		client:                http.DefaultClient,
		dialContext:           nil,
		certMu:                sync.Mutex{},
		ca:                    nil,
		cursorTLSClientConfig: nil,
		rawCaptureSeq:         atomic.Uint64{},
		Tunnels:               newTestTunnelRegistry(),
		captureWriters:        newCaptureWriterCache(logger),
		requestLog:            logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:                    sync.RWMutex{},
		cfg:                   cfg,
		base:                  "",
		server:                nil,
	}
	t.Cleanup(proxy.closeCaptureWriters)
	return proxy
}

func hasWSEvents(lines []string) bool {
	foundStart := false
	foundClientMessage := false
	foundUpstreamMessage := false
	foundCompleted := false
	for _, line := range lines {
		if !strings.Contains(line, `"leg":"`+string(logevent.LegMITMCaptureIndex)+`"`) {
			continue
		}
		if strings.Contains(line, `"phase":"`+string(logevent.PhaseStarted)+`"`) {
			foundStart = true
		}
		if strings.Contains(line, `"direction":"client_to_upstream"`) {
			foundClientMessage = true
		}
		if strings.Contains(line, `"direction":"upstream_to_client"`) {
			foundUpstreamMessage = true
		}
		if strings.Contains(line, `"phase":"`+string(logevent.PhaseCompleted)+`"`) {
			foundCompleted = true
		}
	}
	return foundStart && foundClientMessage && foundUpstreamMessage && foundCompleted
}

// overrideChatGPTUpstream temporarily reroutes the chatGPTUpstream
// constant to the given URL for the duration of the test. Returns
// a cleanup that restores the original.
func overrideChatGPTUpstream(t *testing.T, target string) func() {
	t.Helper()
	original := chatGPTUpstream
	chatGPTUpstream = target
	return func() { chatGPTUpstream = original }
}
