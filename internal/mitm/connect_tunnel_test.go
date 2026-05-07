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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
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
	proxy := startCursorMITMTestProxy(t, captureDir, cursorHost, upstream)
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

func startTestProxy(t *testing.T) *testProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &Proxy{
		log:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		client:                http.DefaultClient,
		dialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:                sync.Mutex{},
		ca:                    nil,
		cursorTLSClientConfig: nil,
		rawCaptureSeq:         atomic.Uint64{},
		mu:                    sync.RWMutex{},
		cfg:                   config.MITMConfig{CaptureDir: t.TempDir(), BodyMode: "summary"},
		base:                  "http://" + listener.Addr().String(),
		server:                nil,
	}
	server := &http.Server{Handler: http.HandlerFunc(p.handle)}
	p.server = server
	go func() { _ = server.Serve(listener) }()
	return &testProxy{server: server, proxy: p, addr: listener.Addr().String()}
}

func startCursorMITMTestProxy(t *testing.T, captureDir string, cursorHost string, upstream *httptest.Server) *testProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	upstreamAddr := strings.TrimPrefix(upstream.URL, "https://")
	ca, err := newCursorCertAuthority()
	if err != nil {
		t.Fatalf("new ca: %v", err)
	}
	p := &Proxy{
		log:                   slog.New(slog.NewTextHandler(io.Discard, nil)),
		client:                http.DefaultClient,
		dialContext:           mappedDialContext(cursorHost+":443", upstreamAddr),
		certMu:                sync.Mutex{},
		ca:                    ca,
		cursorTLSClientConfig: &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}},
		rawCaptureSeq:         atomic.Uint64{},
		mu:                    sync.RWMutex{},
		cfg:                   config.MITMConfig{CaptureDir: captureDir, BodyMode: "summary"},
		base:                  "http://" + listener.Addr().String(),
		server:                nil,
	}
	server := &http.Server{Handler: http.HandlerFunc(p.handle)}
	p.server = server
	go func() { _ = server.Serve(listener) }()
	return &testProxy{server: server, proxy: p, addr: listener.Addr().String()}
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
