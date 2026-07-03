package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm/capture"
)

func TestMitmALPNProtocols(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		offered []string
		want    []string
	}{
		{
			name:    "h2 only",
			offered: []string{http2.NextProtoTLS},
			want:    []string{http2.NextProtoTLS, "http/1.1"},
		},
		{
			name:    "http1 only",
			offered: []string{"http/1.1"},
			want:    []string{http2.NextProtoTLS, "http/1.1"},
		},
		{
			name:    "h2 and http1",
			offered: []string{http2.NextProtoTLS, "http/1.1"},
			want:    []string{http2.NextProtoTLS, "http/1.1"},
		},
		{
			name:    "empty",
			offered: nil,
			want:    nil,
		},
		{
			name:    "exotic only",
			offered: []string{"spdy/3"},
			want:    nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := mitmALPNProtocols(test.offered)
			if !slices.Equal(got, test.want) {
				t.Fatalf("mitmALPNProtocols(%v) = %v want %v", test.offered, got, test.want)
			}
		})
	}
}

// TestProviderTLSHTTP2UpstreamFailureRespondsBadGateway guards against an h2
// stream silently completing as an implicit 200 when the upstream round trip
// fails before any response header is written. Closing the upstream before
// the request forces providerUpstreamRoundTrip to fail; the client must see
// an explicit 502, not net/http2's implicit empty-200 for an unwritten stream.
func TestProviderTLSHTTP2UpstreamFailureRespondsBadGateway(t *testing.T) {
	const providerHost = "api2direct.cursor.sh"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, nil)
	defer proxy.shutdown()
	upstream.Close()

	clientConn, tlsClient, h2Client := connectProviderH2ClientConn(t, proxy, providerHost)
	defer clientConn.Close()
	defer tlsClient.Close()
	defer h2Client.Close()

	req, err := http.NewRequest(http.MethodPost, "https://"+providerHost+"/aiserver.v1.AiService/BidiAppend", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := h2Client.RoundTrip(req)
	if err != nil {
		t.Fatalf("h2 round trip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d want %d (implicit 200 means the failure was silently swallowed)", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestHandleConnectInterceptsCursorTLSHTTP2AndCapturesBodies(t *testing.T) {
	const cursorHost = "api2direct.cursor.sh"
	const requestID = "req_cursor_h2_capture_123"
	const sentinel = "CURSOR_H2_SENTINEL_DO_NOT_LOG"
	upstreamBody := bytes.Repeat([]byte("streaming-cursor-h2-response\n"), 500)
	var upstreamProtoMajor atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamProtoMajor.Store(int32(r.ProtoMajor))
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
		_, _ = w.Write(upstreamBody)
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

	clientConn, tlsClient, h2Client := connectProviderH2ClientConn(t, proxy, cursorHost)
	defer clientConn.Close()
	defer tlsClient.Close()
	defer h2Client.Close()

	requestBody := gzipBytes(t, cursorBidiAppendPayload(requestID, 7, []byte("prefix "+sentinel+" suffix")))
	req, err := http.NewRequest(http.MethodPost, "https://"+cursorHost+"/aiserver.v1.AiService/BidiAppend", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("content-encoding", "gzip")
	req.Header.Set("content-type", "application/protobuf")
	req.Header.Set("authorization", "Bearer should-not-appear-in-jsonl")
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-original-request-id", "orig-h2-123")
	req.Header.Set("x-session-id", "sess-h2-123")
	req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")

	resp, err := h2Client.RoundTrip(req)
	if err != nil {
		t.Fatalf("h2 round trip: %v", err)
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d want %d", resp.StatusCode, http.StatusAccepted)
	}
	if resp.Header.Get("content-type") != "application/protobuf" {
		t.Fatalf("content-type = %q want application/protobuf", resp.Header.Get("content-type"))
	}
	if !bytes.Equal(gotBody, upstreamBody) {
		t.Fatalf("response body mismatch: got %d bytes want %d bytes", len(gotBody), len(upstreamBody))
	}
	if got := upstreamProtoMajor.Load(); got != 1 {
		t.Fatalf("upstream protocol major = %d want 1", got)
	}

	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storedRequest, storedResponse := readStoredCaptureBodies(t, dbPath, requestID)
	if !bytes.Contains(storedRequest, []byte(sentinel)) {
		t.Fatalf("stored request body missing decoded sentinel payload")
	}
	if !bytes.Equal(storedResponse, upstreamBody) {
		t.Fatalf("stored response mismatch: got %d bytes want %d bytes", len(storedResponse), len(upstreamBody))
	}
}

func TestProviderTLSHTTP2ConcurrentStreamsStayIsolated(t *testing.T) {
	const providerHost = "api2direct.cursor.sh"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("content-type", "text/plain")
		_, _ = fmt.Fprintf(w, "path=%s body=%s", r.URL.Path, body)
	}))
	defer upstream.Close()

	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, nil)
	defer proxy.shutdown()

	clientConn, tlsClient, h2Client := connectProviderH2ClientConn(t, proxy, providerHost)
	defer clientConn.Close()
	defer tlsClient.Close()
	defer h2Client.Close()

	var waitGroup sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		i := i
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			path := fmt.Sprintf("/aiserver.v1.AiService/BidiAppend/%d", i)
			body := fmt.Sprintf("body-%d", i)
			req, err := http.NewRequest(http.MethodPost, "https://"+providerHost+path, strings.NewReader(body))
			if err != nil {
				errs <- fmt.Errorf("build request %d: %w", i, err)
				return
			}
			resp, err := h2Client.RoundTrip(req)
			if err != nil {
				errs <- fmt.Errorf("h2 round trip %d: %w", i, err)
				return
			}
			gotBody, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				errs <- fmt.Errorf("read response %d: %w", i, err)
				return
			}
			want := "path=" + path + " body=" + body
			if string(gotBody) != want {
				errs <- fmt.Errorf("response %d = %q want %q", i, gotBody, want)
			}
		}()
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderTLSHTTP2NewStreamsFailDuringDrain(t *testing.T) {
	const providerHost = "api2direct.cursor.sh"
	enteredHold := make(chan struct{})
	releaseHold := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseHold)
		})
	}
	defer release()
	var secondReached atomic.Bool
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			enteredOnce.Do(func() {
				close(enteredHold)
			})
			<-releaseHold
			w.Header().Set("content-type", "text/plain")
			_, _ = w.Write([]byte("held response"))
			return
		}
		if r.URL.Path == "/second" {
			secondReached.Store(true)
		}
		w.Header().Set("content-type", "text/plain")
		_, _ = w.Write([]byte("unexpected upstream response"))
	}))
	defer upstream.Close()

	proxy := startCursorMITMTestProxy(t, t.TempDir(), providerHost, upstream, nil)
	defer proxy.shutdown()
	clientConn, tlsClient, h2Client := connectProviderH2ClientConn(t, proxy, providerHost)
	defer clientConn.Close()
	defer tlsClient.Close()
	defer h2Client.Close()

	firstDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, "https://"+providerHost+"/hold", strings.NewReader("first"))
		if err != nil {
			firstDone <- fmt.Errorf("build first request: %w", err)
			return
		}
		resp, err := h2Client.RoundTrip(req)
		if err != nil {
			firstDone <- fmt.Errorf("first h2 round trip: %w", err)
			return
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			firstDone <- fmt.Errorf("read first response: %w", err)
			return
		}
		if string(body) != "held response" {
			firstDone <- fmt.Errorf("first body = %q want held response", body)
			return
		}
		firstDone <- nil
	}()

	select {
	case <-enteredHold:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for held upstream request")
	}
	waitForExactTunnelCount(t, proxy.proxy, 2, 2*time.Second)

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	drainDone := make(chan struct{})
	go func() {
		proxy.group.Quiesce(drainCtx, "test.reload", livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 50 * time.Millisecond})
		close(drainDone)
	}()
	waitForTunnelState(t, proxy.proxy, livetrack.StateDraining, 2*time.Second)

	secondReq, err := http.NewRequest(http.MethodPost, "https://"+providerHost+"/second", strings.NewReader("second"))
	if err != nil {
		t.Fatalf("build second request: %v", err)
	}
	secondResp, err := h2Client.RoundTrip(secondReq)
	if err != nil {
		t.Fatalf("second h2 round trip during drain: %v", err)
	}
	_, _ = io.Copy(io.Discard, secondResp.Body)
	_ = secondResp.Body.Close()
	if secondResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d want %d", secondResp.StatusCode, http.StatusServiceUnavailable)
	}
	if secondReached.Load() {
		t.Fatal("second request reached upstream during drain")
	}

	release()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-drainDone:
		if got := proxy.proxy.Tunnels.State(); got != livetrack.StateClosed {
			t.Fatalf("drain final state = %s want closed", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for h2 drain to finish")
	}
}

func connectProviderH2ClientConn(t *testing.T, proxy *testProxy, host string) (net.Conn, *tls.Conn, *http2.ClientConn) {
	t.Helper()
	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := fmt.Fprintf(client, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host); err != nil {
		_ = client.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(client)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		_ = client.Close()
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		_ = client.Close()
		t.Fatalf("CONNECT status = %q", strings.TrimSpace(statusLine))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = client.Close()
			t.Fatalf("read CONNECT header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	tlsClient := tls.Client(&bufferedConn{Conn: client, reader: br}, &tls.Config{
		ServerName: host,
		RootCAs:    caPool,
		NextProtos: []string{http2.NextProtoTLS},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		_ = client.Close()
		t.Fatalf("client TLS handshake: %v", err)
	}
	if got := tlsClient.ConnectionState().NegotiatedProtocol; got != http2.NextProtoTLS {
		_ = client.Close()
		t.Fatalf("negotiated protocol = %q want %q", got, http2.NextProtoTLS)
	}
	transport := &http2.Transport{}
	h2Client, err := transport.NewClientConn(tlsClient)
	if err != nil {
		_ = client.Close()
		t.Fatalf("new h2 client conn: %v", err)
	}
	return client, tlsClient, h2Client
}

func readStoredCaptureBodies(t *testing.T, dbPath string, requestID string) ([]byte, []byte) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var storedRequest []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id=?) AND which='request'`, requestID).Scan(&storedRequest); err != nil {
		t.Fatalf("scan request body: %v", err)
	}
	var storedResponse []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id=?) AND which='response'`, requestID).Scan(&storedResponse); err != nil {
		t.Fatalf("scan response body: %v", err)
	}
	return storedRequest, storedResponse
}
