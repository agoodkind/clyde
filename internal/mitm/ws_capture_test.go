package mitm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
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

	logBuffer := &bytes.Buffer{}
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

	logBuffer := &bytes.Buffer{}
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

	logBuffer := &bytes.Buffer{}
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
		Tunnels:         newTestTunnelRegistry(),
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
		Tunnels:         newTestTunnelRegistry(),
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir},
		base:            "",
		server:          nil,
	}
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

// overrideChatGPTUpstream temporarily registers a typed test-only
// [Provider] that claims the legacy ChatGPT backend-api path prefix
// and routes it to the supplied URL.
func overrideChatGPTUpstream(t *testing.T, target string) func() {
	t.Helper()
	return registerTestRoute(t, testRouteOpenAIBackend, target)
}
