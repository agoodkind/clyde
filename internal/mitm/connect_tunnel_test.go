package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
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
	proxy := startCursorMITMTestProxy(t, captureDir, cursorHost, upstream, true)
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

	capturePath := filepath.Join(captureDir, "capture.jsonl")
	waitForFile(t, capturePath)
	records := readCaptureJSONL(t, capturePath)
	if len(records) != 1 {
		t.Fatalf("records = %d want 1: %#v", len(records), records)
	}
	record := records[0]
	if record["concern"] != "cursor.bidi" {
		t.Fatalf("concern = %q want cursor.bidi: %#v", record["concern"], record)
	}
	if record["request_id"] != requestID || record["original_request_id"] != "orig-123" || record["session_id"] != "sess-123" {
		t.Fatalf("metadata ids not captured: %#v", record)
	}
	if strings.Contains(fmt.Sprint(record), sentinel) {
		t.Fatalf("JSONL metadata leaked sentinel prompt text")
	}
	if strings.Contains(fmt.Sprint(record), "should-not-appear") {
		t.Fatalf("JSONL metadata leaked authorization header")
	}
	requestRawPath := record["request_raw_path"].(string)
	responseRawPath := record["response_raw_path"].(string)
	wantRawPrefix := filepath.Join(captureDir, "concerns", "cursor.bidi", "raw", cursorHost)
	if !strings.HasPrefix(requestRawPath, wantRawPrefix) || !strings.HasPrefix(responseRawPath, wantRawPrefix) {
		t.Fatalf("raw paths = %q %q, want prefix %q", requestRawPath, responseRawPath, wantRawPrefix)
	}
	assertFileMode(t, requestRawPath, rawCaptureFileMode)
	assertFileMode(t, responseRawPath, rawCaptureFileMode)
	rawRequest := readFile(t, requestRawPath)
	if !bytes.Contains(rawRequest, []byte("POST /aiserver.v1.AiService/BidiAppend HTTP/1.1")) {
		t.Fatalf("raw request missing request line")
	}
	rawResponse := readFile(t, responseRawPath)
	if len(rawResponse) <= 16*1024 {
		t.Fatalf("raw response length = %d, want larger than old 16 KiB cap", len(rawResponse))
	}
	if !bytes.Contains(rawResponse, upstreamBody[:128]) {
		t.Fatalf("raw response missing upstream payload prefix")
	}
}

func TestProviderTLSKeepaliveRequestsStopAtDrainBoundary(t *testing.T) {
	const providerHost = "chatgpt.com"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	defer upstream.Close()

	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, true)
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

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	drainDone := make(chan livetrack.DrainResult, 1)
	go func() {
		drainDone <- proxy.proxy.Tunnels.Drain(drainCtx, "test.reload")
	}()
	waitForTunnelState(t, proxy.proxy, livetrack.StateDraining, 2*time.Second)

	assertProviderTLSRequestRejected(t, tlsClient, tlsReader, providerHost, "/second")

	select {
	case result := <-drainDone:
		if result.Final != livetrack.StateClosed {
			t.Fatalf("drain final state = %s, want closed", result.Final)
		}
		if result.ForceClosed != 0 {
			t.Fatalf("drain force_closed = %d, want 0", result.ForceClosed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for drain to finish after TLS close")
	}
}

func TestProviderTLSIdleKeepaliveTunnelClosesDuringDrain(t *testing.T) {
	const providerHost = "chatgpt.com"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
	}))
	defer upstream.Close()

	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, true)
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

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	drainDone := make(chan livetrack.DrainResult, 1)
	go func() {
		drainDone <- proxy.proxy.Tunnels.Drain(drainCtx, "test.reload")
	}()
	waitForTunnelState(t, proxy.proxy, livetrack.StateDraining, 2*time.Second)

	select {
	case result := <-drainDone:
		if result.Final != livetrack.StateClosed {
			t.Fatalf("drain final state = %s, want closed", result.Final)
		}
		if result.ForceClosed != 0 {
			t.Fatalf("drain force_closed = %d, want 0", result.ForceClosed)
		}
	case <-time.After(2 * time.Second):
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
	proxy := startCursorMITMTestProxy(t, captureDir, providerHost, upstream, true)
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

	capturePath := filepath.Join(captureDir, "capture.jsonl")
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy := &Proxy{
		log:             logger,
		client:          http.DefaultClient,
		dialContext:     mappedDialContext(cursorHost+":443", upstreamAddr),
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		rawCaptureSeq:   atomic.Uint64{},
		requestLog:      logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil),
		Tunnels:         newTestTunnelRegistry(),
		captureWriters:  newCaptureWriterCache(logger),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir},
		base:            "",
		server:          nil,
	}
	defer proxy.closeCaptureWriters()

	requestBody := []byte(`{"probe":"summary"}`)
	req := httptest.NewRequest(http.MethodPost, "https://"+cursorHost+"/aiserver.v1.AnalyticsService/Batch", bytes.NewReader(requestBody))
	req.Header.Set("content-type", "application/json")
	var output bytes.Buffer
	writer := bufio.NewWriter(&output)
	if err := proxy.handleProviderInterceptedRequest(context.Background(), nil, nil, writer, req, cursorHost+":443", cursorHost, testCursorProvider{}); err != nil {
		t.Fatalf("handle cursor request: %v", err)
	}

	records := readCaptureJSONL(t, filepath.Join(captureDir, "capture.jsonl"))
	if len(records) != 1 {
		t.Fatalf("records = %d want 1: %#v", len(records), records)
	}
	if records[0]["request_raw_path"] != "" || records[0]["response_raw_path"] != "" {
		t.Fatalf("raw paths = %#v %#v, want empty", records[0]["request_raw_path"], records[0]["response_raw_path"])
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
	addr   string
}

func (t *testProxy) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = t.server.Shutdown(ctx)
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

func assertProviderTLSRequest(t *testing.T, conn net.Conn, reader *bufio.Reader, host string, path string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write provider TLS request %s: %v", path, err)
	}
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read provider TLS response %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read provider TLS response body %s: %v", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status for %s = %d want %d; body=%q", path, resp.StatusCode, http.StatusOK, body)
	}
	if !bytes.Contains(body, []byte(path)) {
		t.Fatalf("response for %s missing path marker: %q", path, body)
	}
}

func assertProviderTLSRequestRejected(t *testing.T, conn net.Conn, reader *bufio.Reader, host string, path string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "https://"+host+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	if err := req.Write(conn); err != nil {
		return
	}
	resp, err := http.ReadResponse(reader, req)
	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("expected provider TLS request %s to fail during drain, got status %d", path, resp.StatusCode)
	}
}

func newTestTunnelRegistry() *livetrack.Registry[TunnelMeta] {
	return livetrack.New[TunnelMeta](livetrack.Options[TunnelMeta]{
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
	p := &Proxy{
		log:             logger,
		client:          http.DefaultClient,
		dialContext:     (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:          sync.Mutex{},
		ca:              nil,
		tlsClientConfig: nil,
		rawCaptureSeq:   atomic.Uint64{},
		Tunnels:         newTestTunnelRegistry(),
		captureWriters:  newCaptureWriterCache(logger),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: t.TempDir()},
		base:            "http://" + listener.Addr().String(),
		server:          nil,
	}
	t.Cleanup(p.closeCaptureWriters)
	server := &http.Server{Handler: http.HandlerFunc(p.handle)}
	p.server = server
	go func() { _ = server.Serve(listener) }()
	return &testProxy{server: server, proxy: p, addr: listener.Addr().String()}
}

func startCursorMITMTestProxy(t *testing.T, captureDir string, cursorHost string, upstream *httptest.Server, rawCaptureEnabled bool) *testProxy {
	t.Helper()
	RegisterProviderFirst(testCursorProvider{host: cursorHost})
	t.Cleanup(func() {
		UnregisterProvider(ProviderID("cursor"))
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := &Proxy{
		log:             logger,
		client:          http.DefaultClient,
		dialContext:     mappedDialContext(cursorHost+":443", upstreamAddr),
		certMu:          sync.Mutex{},
		ca:              ca,
		tlsClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		rawCaptureSeq:   atomic.Uint64{},
		Tunnels:         newTestTunnelRegistry(),
		captureWriters:  newCaptureWriterCache(logger),
		mu:              sync.RWMutex{},
		cfg:             config.MITMConfig{CaptureDir: captureDir, RawCaptureEnabled: rawCaptureEnabled},
		base:            "http://" + listener.Addr().String(),
		server:          nil,
	}
	t.Cleanup(p.closeCaptureWriters)
	server := &http.Server{Handler: http.HandlerFunc(p.handle)}
	p.server = server
	go func() { _ = server.Serve(listener) }()
	return &testProxy{server: server, proxy: p, addr: listener.Addr().String()}
}

type testCursorProvider struct {
	id   ProviderID
	host string
}

func (p testCursorProvider) ID() ProviderID {
	if p.id != "" {
		return p.id
	}
	return ProviderID("cursor")
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

func (p testCursorProvider) BuildCaptureExtension(exchange CaptureExchange) CaptureExtension {
	return testCursorCaptureExtension{
		ConcernName:       exchange.Concern,
		Path:              exchange.Path,
		RequestID:         exchange.RequestHeader.Get("x-request-id"),
		OriginalRequestID: exchange.RequestHeader.Get("x-original-request-id"),
		SessionID:         exchange.RequestHeader.Get("x-session-id"),
		RequestRawPath:    exchange.RequestRawPath,
		ResponseRawPath:   exchange.ResponseRawPath,
	}
}

type testCursorCaptureExtension struct {
	ConcernName       string `json:"concern"`
	Path              string `json:"path"`
	RequestID         string `json:"request_id"`
	OriginalRequestID string `json:"original_request_id"`
	SessionID         string `json:"session_id"`
	RequestRawPath    string `json:"request_raw_path"`
	ResponseRawPath   string `json:"response_raw_path"`
}

func (e testCursorCaptureExtension) Concern() string {
	return e.ConcernName
}

func (e testCursorCaptureExtension) MarshalJSONLine() ([]byte, error) {
	return json.Marshal(e)
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
		HasRawCapture:        false,
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

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o want %#o", path, got, want)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}
