package mitm

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/slogger"
)

// syncBuffer is a goroutine-safe bytes.Buffer for tests where the proxy writes
// log output from one goroutine while the test reads it from another. Write and
// String hold the same lock, removing the data race on the shared buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// wsBodySentinel is a unique marker embedded in the websocket frames a capture
// test sends. The upstream echoes it, so both the client and upstream frame
// streams carry it; the wire JSONL must contain it nowhere, proving frame
// content stays out of the log and lives only in the SQLite capture store.
const wsBodySentinel = "ws-body-sentinel-7f3e2d10"

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
	dbPath := filepath.Join(dir, "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close(context.Background(), "test cleanup")
	})
	p := newProxyForTest(t, config.MITMConfig{CaptureDir: dir})
	p.store = store
	p.client = "test"
	// Reroute the codex chatgpt upstream to the test server for the
	// duration of this test.
	defer overrideChatGPTUpstream(t, upstream.URL)()

	// Start the proxy on a random port.
	requestDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(requestDone)
		p.handle(w, r)
	}))
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

	requestFrame := []byte(`{"type":"response.create","probe":"` + wsBodySentinel + `"}`)
	if err := conn.WriteMessage(websocket.TextMessage, requestFrame); err != nil {
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
	waitForWebsocketHandler(t, requestDone)

	// Drain capture file. capture.jsonl is gone; MITM legs now land in
	// this per-concern file.
	capturePath := mitmWireConcernTestPath(dir)
	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	for time.Now().Before(deadline) {
		f, err := os.Open(capturePath)
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
		if err := scanner.Err(); err != nil {
			f.Close()
			t.Fatalf("scan capture file: %v", err)
		}
		f.Close()
		if hasWSEvents(lines) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The wire JSONL must not restate any frame content: neither the client
	// request frame nor the echoed upstream frame (both carry the sentinel).
	// Frame bodies live only in the SQLite capture store.
	rawWire := strings.Join(lines, "\n")
	if strings.Contains(rawWire, wsBodySentinel) {
		t.Fatalf("wire JSONL leaked websocket frame content %q:\n%s", wsBodySentinel, rawWire)
	}

	phases := map[string]int{}
	directions := map[string]int{}
	var completedLeg map[string]any
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse capture line: %v (%q)", err, line)
		}
		if ev["leg"] != string(logevent.LegMITMCaptureIndex) {
			continue
		}
		phase, _ := ev["phase"].(string)
		if phase != "" {
			phases[phase]++
		}
		if phase == string(logevent.PhaseCompleted) {
			completedLeg = ev
		}
		if mitmFields, ok := ev["mitm"].(map[string]any); ok {
			if direction, _ := mitmFields["direction"].(string); direction != "" {
				directions[direction]++
			}
		}
	}

	// The websocket path emits one terminal lifecycle leg per session, with no
	// started leg and no per-frame direction legs. The log states nothing about
	// frame content: no per-direction summary, no sha256, no payload view.
	if phases[string(logevent.PhaseStarted)] != 0 {
		t.Errorf("expected no capture-index started leg, got phases=%v", phases)
	}
	if phases[string(logevent.PhaseCompleted)] != 1 {
		t.Errorf("expected exactly one terminal lifecycle leg, got phases=%v", phases)
	}
	if len(directions) != 0 {
		t.Errorf("expected no per-frame direction legs, got directions=%v", directions)
	}
	if completedLeg == nil {
		t.Fatalf("missing terminal lifecycle leg in %v", lines)
	}
	if _, present := completedLeg["mitm_ws_capture"]; present {
		t.Errorf("terminal leg carries a content summary facet; want none: %#v", completedLeg)
	}
	if _, present := completedLeg["payload_summary"]; present {
		t.Errorf("terminal leg carries a payload summary; want none: %#v", completedLeg)
	}
	if strings.Contains(rawWire, `"sha256"`) {
		t.Errorf("wire JSONL carries a content sha256; want none:\n%s", rawWire)
	}

	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	assertWebsocketCaptureStoreRow(t, dbPath, requestFrame, raw)
}

func TestProxyWebsocketCaptureBridgesCodexRemoteControlPath(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamPath := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath <- r.URL.Path
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upstream upgrade: %v", err)
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := conn.WriteMessage(messageType, payload); err != nil {
			return
		}
	}))
	defer upstream.Close()

	logBuffer := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proxy := newWebsocketRequestLogProxy(t, t.TempDir(), logger)
	defer overrideChatGPTUpstream(t, upstream.URL)()

	srv := httptest.NewServer(http.HandlerFunc(proxy.handle))
	defer srv.Close()

	const remoteControlPath = "/backend-api/wham/remote/control/server"
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + remoteControlPath
	conn, resp, err := websocket.DefaultDialer.DialContext(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"hello":"phone"}`)); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read upstream echo: %v", err)
	}
	_ = conn.Close()

	select {
	case got := <-upstreamPath:
		if got != remoteControlPath {
			t.Fatalf("upstream path = %q, want %q", got, remoteControlPath)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream websocket request")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events := captureLogEvents(t, logBuffer.String(), "logging.request.leg")
		for _, event := range events {
			if event["provider"] == "codex" && event["path"] == remoteControlPath {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("missing codex request leg for %s in logs:\n%s", remoteControlPath, logBuffer.String())
}

func waitForWebsocketHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("websocket handler did not finish after client close")
	}
}

func assertWebsocketCaptureStoreRow(t *testing.T, dbPath string, wantRequest []byte, wantResponse []byte) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var (
		rowID     int64
		client    string
		provider  string
		method    string
		path      string
		status    int
		reqBytes  int
		respBytes int
	)
	row := db.QueryRow(`
		SELECT id, client, provider, method, path, status, req_bytes, resp_bytes
		FROM requests
		WHERE path='/backend-api/codex/responses'
		ORDER BY ts DESC
		LIMIT 1
	`)
	if err := row.Scan(&rowID, &client, &provider, &method, &path, &status, &reqBytes, &respBytes); err != nil {
		t.Fatalf("scan websocket capture row: %v", err)
	}
	if client != "test" {
		t.Fatalf("client = %q want test", client)
	}
	if provider != "codex" {
		t.Fatalf("provider = %q want codex", provider)
	}
	if method != "WEBSOCKET" {
		t.Fatalf("method = %q want WEBSOCKET", method)
	}
	if path != "/backend-api/codex/responses" {
		t.Fatalf("path = %q want /backend-api/codex/responses", path)
	}
	if status != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d want %d", status, http.StatusSwitchingProtocols)
	}
	if reqBytes != len(wantRequest) {
		t.Fatalf("req_bytes = %d want %d", reqBytes, len(wantRequest))
	}
	if respBytes != len(wantResponse) {
		t.Fatalf("resp_bytes = %d want %d", respBytes, len(wantResponse))
	}

	var storedRequest []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=? AND which='request'`, rowID).Scan(&storedRequest); err != nil {
		t.Fatalf("scan request body: %v", err)
	}
	if !bytes.Equal(storedRequest, wantRequest) {
		t.Fatalf("stored request body = %q want %q", storedRequest, wantRequest)
	}
	var storedResponse []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=? AND which='response'`, rowID).Scan(&storedResponse); err != nil {
		t.Fatalf("scan response body: %v", err)
	}
	if !bytes.Equal(storedResponse, wantResponse) {
		t.Fatalf("stored response body = %q want %q", storedResponse, wantResponse)
	}
}

func TestProxyWebsocketCaptureUsesNativeCursorHeadersAndRequiredLegs(t *testing.T) {
	const identityProviderID ProviderID = ProviderIDCursor
	RegisterProviderFirst(testCursorProvider{id: identityProviderID})
	t.Cleanup(func() {
		UnregisterProvider(identityProviderID)
	})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upstream upgrade: %v", err)
		}
		defer conn.Close()
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(messageType, payload); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	logBuffer := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proxy := newWebsocketRequestLogProxy(t, t.TempDir(), logger)
	defer overrideChatGPTUpstream(t, upstream.URL)()

	srv := httptest.NewServer(http.HandlerFunc(proxy.handle))
	defer srv.Close()

	requestHeaders := http.Header{}
	requestHeaders.Set("x-request-id", "cursor-native-req")
	requestHeaders.Set("x-original-request-id", "cursor-native-orig")
	requestHeaders.Set("x-session-id", "cursor-session-1")
	requestHeaders.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/backend-api/codex/responses"
	parsed, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parse ws url: %v", err)
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(context.Background(), parsed.String(), requestHeaders)
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	_ = conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	var events []map[string]any
	for time.Now().Before(deadline) {
		events = captureLogEvents(t, logBuffer.String(), "logging.request.leg")
		seen := make(map[string]bool)
		for _, event := range events {
			if event["surface"] != string(logevent.SurfaceMITMIDE) {
				continue
			}
			if leg, ok := event["leg"].(string); ok {
				seen[leg] = true
			}
		}
		complete := true
		for _, leg := range logevent.DefaultRequiredLegs()[logevent.SurfaceMITMIDE] {
			if !seen[string(leg)] {
				complete = false
				break
			}
		}
		if complete {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	seen := make(map[string]bool)
	var ingressEvent map[string]any
	for _, event := range events {
		if event["surface"] != string(logevent.SurfaceMITMIDE) {
			continue
		}
		if leg, ok := event["leg"].(string); ok {
			seen[leg] = true
			if leg == string(logevent.LegMITMIngress) && ingressEvent == nil {
				ingressEvent = event
			}
		}
	}
	for _, leg := range logevent.DefaultRequiredLegs()[logevent.SurfaceMITMIDE] {
		if !seen[string(leg)] {
			t.Fatalf("missing required leg %s in events %v", leg, seen)
		}
	}
	if ingressEvent == nil {
		t.Fatalf("missing mitm_ingress event in %v", events)
	}
	if ingressEvent["request_id"] != "cursor-native-req" {
		t.Fatalf("request_id = %v want cursor-native-req", ingressEvent["request_id"])
	}
	cursorFacet, ok := ingressEvent["cursor"].(map[string]any)
	if !ok {
		t.Fatalf("cursor facet missing: %#v", ingressEvent)
	}
	if cursorFacet["request_id"] != "cursor-native-req" {
		t.Fatalf("cursor request_id = %v want cursor-native-req", cursorFacet["request_id"])
	}
	if ingressEvent["upstream_request_id"] != "cursor-native-orig" {
		t.Fatalf("upstream_request_id = %v want cursor-native-orig", ingressEvent["upstream_request_id"])
	}
	if ingressEvent["session_id"] != "cursor-session-1" {
		t.Fatalf("session_id = %v want cursor-session-1", ingressEvent["session_id"])
	}
	if ingressEvent["trace_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("trace_id = %v want traceparent trace", ingressEvent["trace_id"])
	}
	if event := firstCaptureLogEvent(t, logBuffer.String(), "logging.request.incomplete"); event != nil {
		t.Fatalf("did not expect incomplete request event: %v", event)
	}
}

func TestProxyWebsocketCaptureEarlyDialFailureEmitsRequestErrorWithoutIncomplete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not a websocket", http.StatusNotFound)
	}))
	defer upstream.Close()

	logBuffer := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	proxy := newWebsocketRequestLogProxy(t, t.TempDir(), logger)
	defer overrideChatGPTUpstream(t, upstream.URL)()

	srv := httptest.NewServer(http.HandlerFunc(proxy.handle))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/backend-api/codex/responses"
	parsed, err := url.Parse(wsURL)
	if err != nil {
		t.Fatalf("parse ws url: %v", err)
	}
	if _, _, err := websocket.DefaultDialer.DialContext(context.Background(), parsed.String(), nil); err == nil {
		t.Fatalf("expected websocket dial to fail")
	}

	deadline := time.Now().Add(2 * time.Second)
	var events []map[string]any
	for time.Now().Before(deadline) {
		events = captureLogEvents(t, logBuffer.String(), "logging.request.leg")
		for _, event := range events {
			if event["leg"] == string(logevent.LegRequestError) {
				deadline = time.Now()
				break
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	foundError := false
	for _, event := range events {
		if event["leg"] == string(logevent.LegRequestError) {
			foundError = true
			if event["error_code"] != "ws_upstream_dial_failed" {
				t.Fatalf("error_code = %v want ws_upstream_dial_failed", event["error_code"])
			}
		}
	}
	if !foundError {
		t.Fatalf("expected request_error leg in %v", events)
	}
	if event := firstCaptureLogEvent(t, logBuffer.String(), "logging.request.incomplete"); event != nil {
		t.Fatalf("did not expect incomplete request event: %v", event)
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
		log:             logger,
		httpClient:      http.DefaultClient,
		dialContext:     nil,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		Tunnels:         newTestTunnelRegistry(livetrack.NewGroup(livetrack.GroupOptions{Log: nil})),
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:              sync.RWMutex{},
		cfg:             cfg,
		base:            "",
		server:          nil,
	}
	return proxy
}

func newWebsocketRequestLogProxy(t *testing.T, captureDir string, logger *slog.Logger) *Proxy {
	t.Helper()
	proxy := &Proxy{
		log:             logger,
		httpClient:      http.DefaultClient,
		dialContext:     nil,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		Tunnels:         newTestTunnelRegistry(livetrack.NewGroup(livetrack.GroupOptions{Log: nil})),
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir},
		base:            "",
		server:          nil,
	}
	return proxy
}

// hasWSEvents reports whether the per-concern wire JSONL contains the single
// terminal websocket capture-index leg a finished bridge emits. The websocket
// path no longer writes a started leg or per-frame direction legs, so detection
// keys on the completed capture-index leg carrying the websocket transport.
func hasWSEvents(lines []string) bool {
	for _, line := range lines {
		if !strings.Contains(line, `"leg":"`+string(logevent.LegMITMCaptureIndex)+`"`) {
			continue
		}
		if !strings.Contains(line, `"phase":"`+string(logevent.PhaseCompleted)+`"`) {
			continue
		}
		if strings.Contains(line, `"transport":"websocket"`) {
			return true
		}
	}
	return false
}

// overrideChatGPTUpstream temporarily registers a typed test-only
// [Provider] that claims the legacy ChatGPT backend-api path prefix
// and routes it to the supplied URL.
func overrideChatGPTUpstream(t *testing.T, target string) func() {
	t.Helper()
	return registerTestRoute(t, testRouteOpenAIBackend, target)
}
