package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"golang.org/x/net/http2"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/slogger"
)

// TestHandleConnectTunnelsBytesBothWays stands up a fake upstream
// that echoes a fixed prefix on connect, accepts a payload from the
// tunneled client, and returns it reversed. The proxy is invoked
// directly through its handle method against a hijackable
// httptest-style listener. Verifies the tunnel:
//
//   - returns "HTTP/1.1 200 Connection Established"
//   - splices client-to-upstream and upstream-to-client bytes
//   - emits the tunnel_open / tunnel_closed log events
func TestHandleConnectTunnelsBytesBothWays(t *testing.T) {
	upstream := startEchoServer(t)
	defer upstream.Close()

	proxy := startTestProxy(t)
	defer proxy.shutdown()

	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()

	if _, err := fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstream.addr, upstream.addr); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	br := bufio.NewReader(client)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		t.Fatalf("status = %q, want 200 Connection Established", strings.TrimSpace(statusLine))
	}
	// Drain headers up to the empty line.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	// Send a payload through the tunnel. The echo upstream will
	// reverse it and write it back.
	payload := "hello-clyde-tunnel"
	if _, err := client.Write([]byte(payload + "\n")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	// Half-close client write so upstream's reader returns EOF and
	// the server's reverse-and-write goroutine flushes.
	if cw, ok := client.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}

	got, err := io.ReadAll(br)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read tunneled response: %v", err)
	}
	want := reverse(payload)
	if !strings.Contains(string(got), want) {
		t.Errorf("tunneled response %q does not contain reversed payload %q", string(got), want)
	}
}

func TestHandleConnectRejectsMissingTarget(t *testing.T) {
	proxy := startTestProxy(t)
	defer proxy.shutdown()

	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if _, err := client.Write([]byte("CONNECT  HTTP/1.1\r\nHost: \r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(client)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Either 400 from our handler or 400/404 from net/http parsing
	// before our handler sees it. Both are acceptable rejections.
	if !strings.Contains(status, "400") && !strings.Contains(status, "404") {
		t.Errorf("missing-target CONNECT status = %q, want 4xx", strings.TrimSpace(status))
	}
}

func TestHandleConnectInterceptsCursorTLSAndCapturesRawFiles(t *testing.T) {
	const cursorHost = "api2.cursor.sh"
	const requestID = "req_cursor_capture_123"
	const sentinel = "CURSOR_SENTINEL_DO_NOT_LOG"
	upstreamBody := bytes.Repeat([]byte("streaming-cursor-response\n"), 900)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("upstream protocol = %s, want HTTP/1.x", r.Proto)
		}
		if r.URL.Path != "/aiserver.v1.AiService/BidiAppend" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		decodedBody, _ := decodeForCapture(body, r.Header.Get("content-encoding"))
		if !bytes.Contains(decodedBody, []byte(sentinel)) {
			t.Errorf("upstream body did not contain sentinel")
		}
		w.Header().Set("content-type", "application/protobuf")
		w.WriteHeader(http.StatusAccepted)
		for i := 0; i < len(upstreamBody); i += 1024 {
			end := i + 1024
			if end > len(upstreamBody) {
				end = len(upstreamBody)
			}
			_, _ = w.Write(upstreamBody[i:end])
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	captureDir := t.TempDir()
	dbPath := filepath.Join(captureDir, "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	proxy := startCursorMITMTestProxy(t, captureDir, cursorHost, upstream, store)
	defer proxy.shutdown()

	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()
	if _, err := fmt.Fprintf(client, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", cursorHost, cursorHost); err != nil {
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
		ServerName: cursorHost,
		RootCAs:    caPool,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	defer tlsClient.Close()

	requestBody := gzipBytes(t, cursorBidiAppendPayload(requestID, 7, []byte("prefix "+sentinel+" suffix")))
	req, err := http.NewRequest(http.MethodPost, "https://"+cursorHost+"/aiserver.v1.AiService/BidiAppend", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("content-encoding", "gzip")
	req.Header.Set("content-type", "application/protobuf")
	req.Header.Set("authorization", "Bearer should-not-appear-in-jsonl")
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-original-request-id", "orig-123")
	req.Header.Set("x-session-id", "sess-123")
	req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	if err := req.Write(tlsClient); err != nil {
		t.Fatalf("write TLS HTTP request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsClient), req)
	if err != nil {
		t.Fatalf("read TLS HTTP response: %v", err)
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d want %d", resp.StatusCode, http.StatusAccepted)
	}
	if len(gotBody) != len(upstreamBody) {
		t.Fatalf("response bytes = %d want %d", len(gotBody), len(upstreamBody))
	}

	// capture.jsonl is gone; MITM legs now land in this per-concern file.
	// recordHTTPCapture runs in the tunnel goroutine after the response is
	// streamed to the client, so the capture-index leg can land a moment
	// after the client read returns; poll until that leg appears.
	capturePath := mitmWireConcernTestPath(captureDir)
	records := waitForCaptureRecordWithLeg(t, capturePath, logevent.LegMITMCaptureIndex)
	// Cursor now rides the same unified leg path as claude and codex:
	// each MITM request emits the LegMITMCaptureIndex record (plus the
	// other leg records) instead of a separate cursor_tls_http record.
	record := firstCaptureRecordWithLeg(t, records, logevent.LegMITMCaptureIndex)
	if record["provider"] != "cursor" {
		t.Fatalf("provider = %q want cursor: %#v", record["provider"], record)
	}
	if record["request_id"] != requestID || record["upstream_request_id"] != "orig-123" || record["session_id"] != "sess-123" {
		t.Fatalf("metadata ids not captured: %#v", record)
	}
	cursorFacet, ok := record["cursor"].(map[string]any)
	if !ok || cursorFacet["request_id"] != requestID {
		t.Fatalf("cursor facet missing request id: %#v", record)
	}
	for _, line := range records {
		if strings.Contains(fmt.Sprint(line), sentinel) {
			t.Fatalf("JSONL metadata leaked sentinel prompt text: %#v", line)
		}
		if strings.Contains(fmt.Sprint(line), "should-not-appear") {
			t.Fatalf("JSONL metadata leaked authorization header: %#v", line)
		}
	}
	// The full decoded request/response bodies persist to the SQLite capture
	// store, not to per-leg .raw files. Wait for the row before closing: the
	// store's Record is asynchronous and a Record that arrives after Close is
	// dropped, which would leave this query with nothing to scan.
	waitForStoredRequestRow(t, dbPath, requestID)
	// Close flushes the writer queue and checkpoints the WAL into the main
	// database file so the verifier handle below sees every committed row.
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var (
		gotClient   string
		gotProvider string
		gotConcern  string
	)
	row := db.QueryRow(`SELECT client, provider, concern FROM requests WHERE request_id=? ORDER BY ts DESC LIMIT 1`, requestID)
	if err := row.Scan(&gotClient, &gotProvider, &gotConcern); err != nil {
		t.Fatalf("scan request row: %v", err)
	}
	if gotClient != "test" {
		t.Fatalf("client = %q want test", gotClient)
	}
	if gotProvider != "cursor" {
		t.Fatalf("provider = %q want cursor", gotProvider)
	}
	if gotConcern != "cursor.bidi" {
		t.Fatalf("concern = %q want cursor.bidi", gotConcern)
	}

	var storedResponse []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id=?) AND which='response'`, requestID).Scan(&storedResponse); err != nil {
		t.Fatalf("scan response body: %v", err)
	}
	if len(storedResponse) != len(upstreamBody) {
		t.Fatalf("stored response length = %d want %d (full body, not the old 16 KiB cap)", len(storedResponse), len(upstreamBody))
	}
	if !bytes.Contains(storedResponse, upstreamBody[:128]) {
		t.Fatalf("stored response missing upstream payload prefix")
	}

	var storedRequest []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id=?) AND which='request'`, requestID).Scan(&storedRequest); err != nil {
		t.Fatalf("scan request body: %v", err)
	}
	// The proxy decodes the gzip content-encoding before storing, so the
	// sentinel prompt text is present in cleartext in the stored request body.
	if !bytes.Contains(storedRequest, []byte(sentinel)) {
		t.Fatalf("stored request body missing decoded sentinel payload")
	}
}

func TestProviderTLSKeepaliveRequestsStopAtDrainBoundary(t *testing.T) {
	const providerHost = "chatgpt.com"
	gate := newHeldUpstreamGate()
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == heldUpstreamPath {
			gate.hold()
		}
		w.Header().Set("content-type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	defer upstream.Close()
	// Released before upstream.Close because defers unwind last-registered
	// first: a handler still parked in the gate would block Close forever if
	// the test failed before its own release.
	defer gate.releaseAll()

	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, nil)
	defer proxy.shutdown()

	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()
	if _, err := fmt.Fprintf(client, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", providerHost, providerHost); err != nil {
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
		ServerName: providerHost,
		RootCAs:    caPool,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	defer tlsClient.Close()
	tlsReader := bufio.NewReader(tlsClient)

	assertProviderTLSRequest(t, tlsClient, tlsReader, providerHost, "/first")
	waitForExactTunnelCount(t, proxy.proxy, 1, 2*time.Second)

	// Park a request inside the upstream handler and leave it there across the
	// drain. Its per-request livetrack session stays registered and the
	// tunnel's drain watcher sees a non-zero in-flight count, so the registry
	// holds StateDraining until this test releases the request instead of
	// passing through it on the way to StateClosed.
	held := make(chan providerTLSResponse, 1)
	go func() {
		held <- requestProviderTLS(tlsClient, tlsReader, providerHost, heldUpstreamPath)
	}()
	<-gate.entered

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainBudgetCap)
	defer cancelDrain()
	// Drive the drain through the lifecycle group's Quiesce, the only public
	// drain entry point. The tunnel registry is the group's sole member here, so
	// Quiesce drains it and transitions it to StateClosed once the in-flight
	// request completes and the keepalive loop exits. The cap is far longer than
	// the exchange needs, so reaching StateClosed is the natural (non-force)
	// completion path; the exact force-closed count is unit-tested in
	// livetrack/drain_test.go.
	drainDone := make(chan struct{})
	go func() {
		proxy.group.Quiesce(drainCtx, "test.reload", livetrack.Budget{Cap: drainBudgetCap, IdleGrace: 0})
		close(drainDone)
	}()
	waitForTunnelState(t, proxy.proxy, livetrack.StateDraining, 2*time.Second)

	// A request already in flight when the drain begins runs to completion.
	gate.releaseAll()
	inFlight := <-held
	if inFlight.err != nil {
		t.Fatalf("in-flight request across the drain boundary: %v", inFlight.err)
	}
	if !bytes.Contains(inFlight.body, []byte(heldUpstreamPath)) {
		t.Fatalf("in-flight response missing path marker: %q", inFlight.body)
	}

	// The keepalive loop stops at the drain boundary once that request
	// finishes, so the next request on the same tunnel is never served.
	assertProviderTLSRequestRejected(t, tlsClient, tlsReader, providerHost, "/second")

	select {
	case <-drainDone:
		if got := proxy.proxy.Tunnels.State(); got != livetrack.StateClosed {
			t.Fatalf("drain final state = %s, want closed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for drain to finish after TLS close")
	}
}

// TestProviderTLSAcceptsH2OnlyALPNOffer verifies the intercepting TLS server
// negotiates h2 when a client offers only h2 in its ClientHello. The dynamic
// ALPN selection remains generic for every claimed host, so this test dials
// with an arbitrary provider host rather than any specific hostname.
func TestProviderTLSAcceptsH2OnlyALPNOffer(t *testing.T) {
	const providerHost = "chatgpt.com"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	defer upstream.Close()

	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, nil)
	defer proxy.shutdown()

	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()
	if _, err := fmt.Fprintf(client, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", providerHost, providerHost); err != nil {
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
		ServerName: providerHost,
		RootCAs:    caPool,
		NextProtos: []string{"h2"},
		MinVersion: tls.VersionTLS12,
	})
	defer tlsClient.Close()
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("client TLS handshake with h2-only ALPN offer failed: %v", err)
	}
	if got := tlsClient.ConnectionState().NegotiatedProtocol; got != http2.NextProtoTLS {
		t.Fatalf("negotiated protocol = %q want %q", got, http2.NextProtoTLS)
	}
}

func TestProviderTLSIdleKeepaliveTunnelClosesDuringDrain(t *testing.T) {
	const providerHost = "chatgpt.com"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	defer upstream.Close()

	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, nil)
	defer proxy.shutdown()

	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()
	if _, err := fmt.Fprintf(client, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", providerHost, providerHost); err != nil {
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
		ServerName: providerHost,
		RootCAs:    caPool,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	defer tlsClient.Close()
	tlsReader := bufio.NewReader(tlsClient)

	assertProviderTLSRequest(t, tlsClient, tlsReader, providerHost, "/first")
	waitForExactTunnelCount(t, proxy.proxy, 1, 2*time.Second)

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainBudgetCap)
	defer cancelDrain()
	// Drive the drain through the lifecycle group's Quiesce, the only public
	// drain entry point. With an idle keepalive tunnel and idle-grace set, the
	// tunnel registry closes the wedged tunnel and reaches StateClosed well
	// inside the budget cap. The cap is deliberately far above the wait below,
	// so finishing at all is what proves the tunnel was closed on the drain's
	// own initiative rather than by the budget expiring.
	//
	// There is no wait for StateDraining here: it is a state the registry
	// passes through, and the terminal StateClosed asserted below is only
	// reachable through it.
	drainDone := make(chan struct{})
	go func() {
		proxy.group.Quiesce(drainCtx, "test.reload", livetrack.Budget{Cap: drainBudgetCap, IdleGrace: 50 * time.Millisecond})
		close(drainDone)
	}()

	select {
	case <-drainDone:
		if got := proxy.proxy.Tunnels.State(); got != livetrack.StateClosed {
			t.Fatalf("drain final state = %s, want closed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for idle provider TLS tunnel to close during drain")
	}
}

func TestHandleConnectInterceptsProviderTLSWebsocket(t *testing.T) {
	const providerHost = "chatgpt.com"
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read: %v", err)
			return
		}
		if messageType != websocket.TextMessage {
			t.Errorf("message type = %d, want text", messageType)
		}
		if string(payload) != `{"type":"response.create"}` {
			t.Errorf("payload = %s", payload)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)); err != nil {
			t.Errorf("upstream write: %v", err)
		}
	}))
	defer upstream.Close()

	captureDir := t.TempDir()
	proxy := startCursorMITMTestProxy(t, captureDir, providerHost, upstream, nil)
	defer proxy.shutdown()
	logger := slog.New(newMITMCaptureTestHandler(t, captureDir))
	proxy.proxy.log = logger
	proxy.proxy.requestLog = logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil)

	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	proxyURL := &url.URL{Scheme: "http", Host: proxy.addr}
	dialer := websocket.Dialer{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs:    caPool,
			ServerName: providerHost,
			NextProtos: []string{"http/1.1"},
		},
		HandshakeTimeout: 2 * time.Second,
	}
	conn, resp, err := dialer.Dial("wss://"+providerHost+"/backend-api/codex/responses", nil)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial websocket through CONNECT: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("message type = %d, want text", messageType)
	}
	if string(payload) != `{"type":"response.completed"}` {
		t.Fatalf("payload = %s", payload)
	}
	if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
		t.Fatalf("write websocket close: %v", err)
	}

	// capture.jsonl is gone; MITM legs now land in this per-concern file.
	capturePath := mitmWireConcernTestPath(captureDir)
	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	foundCapture := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(capturePath); err == nil {
			lines = strings.Split(string(readFile(t, capturePath)), "\n")
			if hasWSEvents(lines) {
				foundCapture = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !foundCapture {
		t.Fatalf("websocket capture records were not written: %v", lines)
	}
	_ = conn.Close()
	waitForExactTunnelCount(t, proxy.proxy, 0, 2*time.Second)
}

func TestHandleConnectInterceptsCursorTLSAndSkipsRawFilesWhenDisabled(t *testing.T) {
	const cursorHost = "api2.cursor.sh"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	captureDir := t.TempDir()
	upstreamAddr := strings.TrimPrefix(upstream.URL, "https://")
	logger := slog.New(newMITMCaptureTestHandler(t, captureDir))
	proxy := &Proxy{
		log:             logger,
		httpClient:      http.DefaultClient,
		dialContext:     mappedDialContext(cursorHost+":443", upstreamAddr),
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		store:           nil,
		client:          "test",
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		Tunnels:         newTestTunnelRegistry(livetrack.NewGroup(livetrack.GroupOptions{Log: nil})),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir},
		base:            "",
		server:          nil,
		h2Server:        &http2.Server{},
	}

	requestBody := []byte(`{"probe":"summary"}`)
	req := httptest.NewRequest(http.MethodPost, "https://"+cursorHost+"/aiserver.v1.AnalyticsService/Batch", bytes.NewReader(requestBody))
	req.Header.Set("content-type", "application/json")
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	sink := &bufioProviderResponseSink{proxy: proxy, bufw: writer}
	if err := proxy.handleProviderInterceptedRequest(context.Background(), nil, nil, sink, req, cursorHost+":443", cursorHost, testCursorProvider{}, nil, nil); err != nil {
		t.Fatalf("handle cursor request: %v", err)
	}

	// capture.jsonl is gone; MITM legs now land in this per-concern file.
	records := readCaptureJSONL(t, mitmWireConcernTestPath(captureDir))
	record := firstCaptureRecordWithLeg(t, records, logevent.LegMITMCaptureIndex)
	mitmFields := mitmFieldsFromCaptureRecord(t, record)
	if mitmFields["raw_request_path"] != nil || mitmFields["raw_response_path"] != nil {
		t.Fatalf("raw paths = %#v %#v, want absent", mitmFields["raw_request_path"], mitmFields["raw_response_path"])
	}
	if _, err := os.Stat(filepath.Join(captureDir, "concerns", "unknown", "raw")); !os.IsNotExist(err) {
		t.Fatalf("raw dir stat err=%v want not exist", err)
	}
	if !strings.Contains(output.String(), "HTTP/1.1 200 OK") {
		t.Fatalf("response missing status: %q", output.String())
	}
}

// echoServer accepts TCP connections, reads a line, and writes back
// the reversed line. Used as a tunneled upstream in proxy tests.
type echoServer struct {
	listener net.Listener
	addr     string
	wg       sync.WaitGroup
}

func startEchoServer(t *testing.T) *echoServer {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &echoServer{listener: listener, addr: listener.Addr().String()}
	server.wg.Add(1)
	go server.serve()
	return server
}

func (s *echoServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			_, _ = c.Write([]byte(reverse(line)))
		}(conn)
	}
}

func (s *echoServer) Close() {
	_ = s.listener.Close()
	s.wg.Wait()
}

type testProxy struct {
	server *http.Server
	proxy  *Proxy
	group  *livetrack.Group
	addr   string
}

func (t *testProxy) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = t.server.Shutdown(ctx)
	if t.group != nil {
		// Drain the tunnel registry so intercepted-stream goroutines finish
		// their wire-leg logging before the test's t.TempDir cleanup runs.
		// Otherwise a late log write into <captureDir>/providers/mitm races
		// t.TempDir's RemoveAll and fails with "directory not empty". Quiesce
		// on an already-drained group is a no-op, so tests that drain the group
		// themselves before calling shutdown stay correct.
		t.group.Quiesce(ctx, "test.shutdown", livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0})
	}
	if t.proxy != nil {
		_ = t.proxy.ShutdownQUIC(ctx)
		_ = t.proxy.CloseQUICTransport()
	}
}

func waitForExactTunnelCount(t *testing.T, proxy *Proxy, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if proxy.Tunnels.Count() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Tunnels.Count: got %d, want %d after %s", proxy.Tunnels.Count(), want, timeout)
}

func waitForTunnelState(t *testing.T, proxy *Proxy, want livetrack.State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if proxy.Tunnels.State() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Tunnels.State: got %s, want %s after %s", proxy.Tunnels.State(), want, timeout)
}

// drainBudgetCap bounds the drain tests' Quiesce budget. It is set far
// above what any of those exchanges need so the deadline force-close
// path never fires incidentally: a drain that reaches StateClosed inside
// this cap did so because its sessions were released, not because the
// budget ran out.
const drainBudgetCap = 30 * time.Second

// heldUpstreamPath is the request path whose upstream handler parks in
// [heldUpstreamGate] until the test releases it.
const heldUpstreamPath = "/hold"

// heldUpstreamGate lets a test hold one upstream request open for as
// long as it wants. A drain test uses it to keep a request genuinely in
// flight, which pins the tunnel registry in StateDraining rather than
// leaving the test to sample a state the registry only passes through.
type heldUpstreamGate struct {
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newHeldUpstreamGate() *heldUpstreamGate {
	return &heldUpstreamGate{
		entered:     make(chan struct{}, 1),
		release:     make(chan struct{}),
		releaseOnce: sync.Once{},
	}
}

// hold signals that the upstream handler has been entered and blocks
// until releaseAll runs. The entered channel is buffered and the send is
// non-blocking, so a handler that runs more than once never wedges on a
// test that only waits for the first entry.
func (g *heldUpstreamGate) hold() {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.release
}

// releaseAll unblocks every parked handler. It is idempotent so a test
// can release explicitly and still defer it as a cleanup guard.
func (g *heldUpstreamGate) releaseAll() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// providerTLSResponse is the outcome of one request written over an
// intercepted provider TLS connection. [requestProviderTLS] runs on a
// helper goroutine in the drain test, where t.Fatalf is not legal, so
// the outcome travels back as a value.
type providerTLSResponse struct {
	body []byte
	err  error
}

func requestProviderTLS(conn net.Conn, reader *bufio.Reader, host string, path string) providerTLSResponse {
	req, err := http.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		return providerTLSResponse{body: nil, err: fmt.Errorf("build request %s: %w", path, err)}
	}
	if err := req.Write(conn); err != nil {
		return providerTLSResponse{body: nil, err: fmt.Errorf("write provider TLS request %s: %w", path, err)}
	}
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return providerTLSResponse{body: nil, err: fmt.Errorf("read provider TLS response %s: %w", path, err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return providerTLSResponse{body: nil, err: fmt.Errorf("read provider TLS response body %s: %w", path, err)}
	}
	if resp.StatusCode != http.StatusOK {
		return providerTLSResponse{body: body, err: fmt.Errorf("status for %s = %d want %d; body=%q", path, resp.StatusCode, http.StatusOK, body)}
	}
	return providerTLSResponse{body: body, err: nil}
}

func assertProviderTLSRequest(t *testing.T, conn net.Conn, reader *bufio.Reader, host string, path string) {
	t.Helper()
	result := requestProviderTLS(conn, reader, host, path)
	if result.err != nil {
		t.Fatalf("provider TLS exchange: %v", result.err)
	}
	if !bytes.Contains(result.body, []byte(path)) {
		t.Fatalf("response for %s missing path marker: %q", path, result.body)
	}
}

func assertProviderTLSRequestRejected(t *testing.T, conn net.Conn, reader *bufio.Reader, host string, path string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// The deadline is a hang guard rather than the mechanism under test: the
	// drain closes the tunnel, so the read fails as soon as the peer's FIN
	// lands. A timeout means the tunnel stayed open and simply served nothing,
	// which is a different behavior, so it fails instead of passing as a
	// rejection.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	if err := req.Write(conn); err != nil {
		return
	}
	resp, err := http.ReadResponse(reader, req)
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("expected provider TLS request %s to fail during drain, got status %d", path, resp.StatusCode)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("provider TLS request %s was neither served nor refused: the read timed out with the tunnel still open", path)
	}
}

func newTestTunnelRegistry(group *livetrack.Group) *livetrack.Registry[TunnelMeta] {
	return livetrack.Attach[TunnelMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseIngress,
		QuietRelevant: true,
		CancelNoWait:  false,
	}, livetrack.Options[TunnelMeta]{
		Component:     "mitm",
		Concern:       "providers.mitm.lifecycle",
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollEvery:     5 * time.Millisecond,
		CloserGrace:   200 * time.Millisecond,
		ParallelClose: false,
		Now:           nil,
	})
}

func startTestProxy(t *testing.T) *testProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	p := &Proxy{
		log:             logger,
		httpClient:      http.DefaultClient,
		dialContext:     (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		store:           nil,
		client:          "test",
		Tunnels:         newTestTunnelRegistry(group),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: t.TempDir()},
		base:            "http://" + listener.Addr().String(),
		server:          nil,
		h2Server:        &http2.Server{},
	}
	server := &http.Server{Handler: http.HandlerFunc(p.handle)}
	p.server = server
	go func() { _ = server.Serve(newSniffListener(context.Background(), listener, p)) }()
	return &testProxy{server: server, proxy: p, group: group, addr: listener.Addr().String()}
}

func startCursorMITMTestProxy(t *testing.T, captureDir string, cursorHost string, upstream *httptest.Server, store *capture.Store) *testProxy {
	t.Helper()
	RegisterProviderFirst(testCursorProvider{host: cursorHost})
	t.Cleanup(func() {
		UnregisterProvider(ProviderIDCursor)
	})
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	upstreamAddr := strings.TrimPrefix(upstream.URL, "https://")
	caDir := t.TempDir()
	ca, err := loadOrCreateCertAuthority(
		filepath.Join(caDir, "ca.crt"),
		filepath.Join(caDir, "ca.key"),
		time.Now,
	)
	if err != nil {
		t.Fatalf("load ca: %v", err)
	}
	// MITM wire legs land in the per-concern providers/mitm/wire.jsonl file;
	// full request/response bodies are persisted to the shared SQLite capture
	// store. The proxy no longer owns a separate raw-file writer.
	logger := slog.New(newMITMCaptureTestHandler(t, captureDir))
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	p := &Proxy{
		log:             logger,
		httpClient:      http.DefaultClient,
		dialContext:     mappedDialContext(cursorHost+":443", upstreamAddr),
		certMu:          sync.Mutex{},
		ca:              ca,
		tlsClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		store:           store,
		client:          "test",
		Tunnels:         newTestTunnelRegistry(group),
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir},
		base:            "http://" + listener.Addr().String(),
		server:          nil,
		h2Server:        &http2.Server{},
	}
	server := &http.Server{Handler: http.HandlerFunc(p.handle)}
	p.server = server
	go func() { _ = server.Serve(newSniffListener(context.Background(), listener, p)) }()
	return &testProxy{server: server, proxy: p, group: group, addr: listener.Addr().String()}
}

type testCursorProvider struct {
	id   ProviderID
	host string
}

func (p testCursorProvider) ID() ProviderID {
	if p.id != ProviderIDUnknown {
		return p.id
	}
	return ProviderIDCursor
}

func (p testCursorProvider) ClassifyConnect(host string) ConnectClaim {
	return ConnectClaim{
		Claimed:    host == p.host || p.host == "",
		Host:       host,
		ProviderID: p.ID(),
	}
}

func (p testCursorProvider) ClassifyPlain(path string) PlainRouteClaim {
	return PlainRouteClaim{
		Claimed:     false,
		Provider:    "",
		UpstreamURL: "",
	}
}

func (p testCursorProvider) ExtractIdentity(headers http.Header) IdentityContribution {
	facet := testCursorFacet{
		RequestID:         headers.Get("x-request-id"),
		OriginalRequestID: headers.Get("x-original-request-id"),
		SessionID:         headers.Get("x-session-id"),
	}
	return IdentityContribution{
		PreferredRequestID:         headers.Get("x-request-id"),
		PreferredUpstreamRequestID: headers.Get("x-original-request-id"),
		SessionID:                  headers.Get("x-session-id"),
		Facet:                      facet,
	}
}

type testCursorFacet struct {
	RequestID         string
	OriginalRequestID string
	SessionID         string
}

func (f testCursorFacet) FacetKey() string {
	return "cursor"
}

func (f testCursorFacet) FacetAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("request_id", f.RequestID),
		slog.String("original_request_id", f.OriginalRequestID),
		slog.String("session_id", f.SessionID),
	}
}

func (f testCursorFacet) SinkHints() logevent.SinkHints {
	return logevent.SinkHints{
		NeedsProviderSidecar: false,
	}
}

func mappedDialContext(from string, to string) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		if address == from {
			address = to
		}
		return dialer.DialContext(ctx, network, address)
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func cursorBidiAppendPayload(requestID string, seqno uint64, payload []byte) []byte {
	var out []byte
	out = appendProtoString(out, 1, requestID)
	out = appendProtoVarint(out, 2, seqno)
	out = appendProtoBytes(out, 3, payload)
	return out
}

func readCaptureJSONL(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw := readFile(t, path)
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("unmarshal capture record: %v\n%s", err, line)
		}
		records = append(records, record)
	}
	return records
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

// waitForCaptureRecordWithLeg polls the per-concern wire file until at
// least one JSONL record carries the wanted leg, then returns every
// parsed record. The capture leg is emitted from the tunnel goroutine
// after the response is streamed to the client, so a bare file-exists
// wait can race ahead of the capture-index write.
func waitForCaptureRecordWithLeg(t *testing.T, path string, leg logevent.Leg) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			records := make([]map[string]any, 0)
			for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
				if len(line) == 0 {
					continue
				}
				var record map[string]any
				if err := json.Unmarshal(line, &record); err != nil {
					continue
				}
				records = append(records, record)
			}
			for _, record := range records {
				if record["leg"] == string(leg) {
					return records
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for capture record with leg %s at %s", leg, path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForCaptureCommit polls the capture database until probe reports
// the awaited row is committed, using a second read-only handle while
// the store stays open.
//
// The capture store's writer is asynchronous and Record drops silently
// once the store is closed, so closing the store is only safe after the
// exchange has handed its record over. No wire leg marks that hand-off:
// the intercepted-TLS path emits every leg, including the capture-index
// leg, and only then calls into the store. Waiting on the row itself is
// the one signal that the record cannot still be in flight.
func waitForCaptureCommit(t *testing.T, dbPath string, what string, probe func(*sql.DB) (bool, error)) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open capture reader: %v", err)
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		found, probeErr := probe(db)
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s in %s: %v", what, dbPath, probeErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForStoredRequestRow blocks until the capture store has committed
// the request row carrying requestID.
func waitForStoredRequestRow(t *testing.T, dbPath string, requestID string) {
	t.Helper()
	waitForCaptureCommit(t, dbPath, "capture row for request "+requestID, func(db *sql.DB) (bool, error) {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM requests WHERE request_id=?`, requestID).Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	})
}

func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// spliceIdleStep is how far the splice test advances its manual clock
// to make a session look idle. The value is virtual time, so it costs
// the test nothing to wait.
const spliceIdleStep = 250 * time.Millisecond

// manualClock is a race-safe time source whose value moves only when the
// test advances it. livetrack reads it through Options.Now, so both the
// session's recorded activity and the idle measurement come from it.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(start time.Time) *manualClock {
	return &manualClock{mu: sync.Mutex{}, now: start}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(step time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(step)
}

// waitForSessionTouch blocks until the session's recorded activity
// catches up with the manual clock, which happens only when Touch fires.
// The clock stands still while it waits, so the condition latches: once
// idle reaches zero it stays there, and a poll goroutine that loses the
// CPU for an arbitrary stretch still observes it. The deadline can only
// expire if the Touch never happened at all.
func waitForSessionTouch(t *testing.T, sess *livetrack.Session[TunnelMeta], clock *manualClock, direction string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if sess.IdleSince(clock.Now()) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s touch never observed: idle=%v", direction, sess.IdleSince(clock.Now()))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSpliceConnectionsTouchesSessionOnWrite drives a pair of
// net.Pipe connections through spliceConnections, sends bytes in one
// direction at a time, and asserts the registered livetrack session's
// activity timestamp catches up after each successful write. The
// test exists so a refactor that drops the touchOnWrite wrapper from
// spliceConnections fails here instead of silently regressing the
// daemon reload-drain idle-grace behavior.
//
// The registry runs on a manual clock because idle time grows on its
// own: an assertion phrased as "idle is small right now" is only true
// inside a window, and a poll loop that gets descheduled past that
// window reports a touch that did happen as one that never did. Freezing
// the clock turns each expected touch into idle == 0, which stays true
// until the test advances the clock itself.
func TestSpliceConnectionsTouchesSessionOnWrite(t *testing.T) {
	t.Parallel()
	clock := newManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	registry := livetrack.Attach[TunnelMeta](livetrack.NewGroup(livetrack.GroupOptions{Log: nil}), livetrack.MemberSpec{
		Phase:         livetrack.PhaseIngress,
		QuietRelevant: true,
		CancelNoWait:  false,
	}, livetrack.Options[TunnelMeta]{
		Component:     "test",
		Concern:       "test.splice.touch",
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		PollEvery:     5 * time.Millisecond,
		CloserGrace:   100 * time.Millisecond,
		ParallelClose: false,
		Now:           clock.Now,
	})
	clientLocal, clientRemote := net.Pipe()
	upstreamLocal, upstreamRemote := net.Pipe()
	sess, err := registry.Register(context.Background(), "splice.test", TunnelMeta{
		ConnectHost:   "test.host",
		UpstreamAddr:  "test.host:443",
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, newTunnelCloser(&connCloser{conn: clientLocal}, &connCloser{conn: upstreamLocal}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Register stamped the session's activity at the current clock value, so
	// advancing the clock with no byte flow is what makes it look idle. That
	// is the same measurement the drain idle-grace fast-path takes.
	clock.Advance(spliceIdleStep)
	if before := sess.IdleSince(clock.Now()); before != spliceIdleStep {
		t.Fatalf("pre-splice idle: got %v, want %v", before, spliceIdleStep)
	}

	spliceDone := make(chan struct{})
	go func() {
		defer close(spliceDone)
		_, _ = spliceConnections(clientLocal, upstreamLocal, sess)
	}()

	// Send one byte from client to upstream and drain it on the
	// remote side. The corresponding spliceConnections goroutine
	// writes into upstreamLocal via touchOnWrite, which fires
	// sess.Touch on success.
	go func() {
		_, _ = clientRemote.Write([]byte("c"))
	}()
	readBuf := make([]byte, 1)
	if _, err := upstreamRemote.Read(readBuf); err != nil {
		t.Fatalf("upstream read after client write: %v", err)
	}
	waitForSessionTouch(t, sess, clock, "client->upstream")

	// Now drive a byte the other direction and confirm Touch fires
	// from the downstream goroutine too. The clock step first proves
	// nothing else refreshes the session while no bytes move.
	clock.Advance(spliceIdleStep)
	if mid := sess.IdleSince(clock.Now()); mid != spliceIdleStep {
		t.Fatalf("idle during silence: got %v, want %v", mid, spliceIdleStep)
	}
	go func() {
		_, _ = upstreamRemote.Write([]byte("u"))
	}()
	if _, err := clientRemote.Read(readBuf); err != nil {
		t.Fatalf("client read after upstream write: %v", err)
	}
	waitForSessionTouch(t, sess, clock, "upstream->client")

	// Close both ends so spliceConnections returns. clientRemote
	// and upstreamRemote close together; the spliceConnections
	// goroutines exit when both io.Copy calls return.
	_ = clientRemote.Close()
	_ = upstreamRemote.Close()
	_ = clientLocal.Close()
	_ = upstreamLocal.Close()
	select {
	case <-spliceDone:
	case <-time.After(2 * time.Second):
		t.Fatal("spliceConnections did not return after closing pipes")
	}
}
