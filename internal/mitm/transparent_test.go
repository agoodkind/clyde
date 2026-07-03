package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
)

func TestTransparentRawTLSInterceptsAndCaptures(t *testing.T) {
	const providerHost = "api2direct.cursor.sh"
	const requestID = "req_cursor_transparent_h2_capture_123"
	const sentinel = "TRANSPARENT_H2_SENTINEL_DO_NOT_LOG"
	requestBody := []byte("prefix " + sentinel + " suffix")
	upstreamBody := []byte(`{"ok":true,"transport":"transparent-h2"}`)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aiserver.v1.AiService/BidiAppend" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if !bytes.Contains(body, []byte(sentinel)) {
			t.Errorf("upstream body did not contain sentinel")
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(upstreamBody)
	}))
	defer upstream.Close()

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
	proxy := startCursorMITMTestProxy(t, captureDir, providerHost, upstream, store)
	defer proxy.shutdown()

	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	rawClient, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = rawClient.Close() }()
	tlsClient := tls.Client(rawClient, &tls.Config{
		ServerName: providerHost,
		RootCAs:    caPool,
		NextProtos: []string{http2.NextProtoTLS},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	defer func() { _ = tlsClient.Close() }()
	if got := tlsClient.ConnectionState().NegotiatedProtocol; got != http2.NextProtoTLS {
		t.Fatalf("negotiated protocol = %q want %q", got, http2.NextProtoTLS)
	}

	transport := &http2.Transport{}
	h2Client, err := transport.NewClientConn(tlsClient)
	if err != nil {
		t.Fatalf("new h2 client conn: %v", err)
	}
	defer h2Client.Close()
	req, err := http.NewRequest(http.MethodPost, "https://"+providerHost+"/aiserver.v1.AiService/BidiAppend", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("content-type", "application/protobuf")
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-original-request-id", "orig-transparent-123")
	req.Header.Set("x-session-id", "sess-transparent-123")
	resp, err := h2Client.RoundTrip(req)
	if err != nil {
		t.Fatalf("h2 round trip: %v", err)
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d want %d", resp.StatusCode, http.StatusCreated)
	}
	if !bytes.Equal(gotBody, upstreamBody) {
		t.Fatalf("response body = %q want %q", gotBody, upstreamBody)
	}

	records := waitForCaptureRecordWithLeg(t, mitmWireConcernTestPath(captureDir), logevent.LegMITMCaptureIndex)
	record := firstCaptureRecordWithLeg(t, records, logevent.LegMITMCaptureIndex)
	if record["provider"] != "cursor" {
		t.Fatalf("provider = %q want cursor: %#v", record["provider"], record)
	}
	if record["request_id"] != requestID {
		t.Fatalf("request_id = %q want %q: %#v", record["request_id"], requestID, record)
	}
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storeOpen = false
	storedRequest, storedResponse := readStoredCaptureBodies(t, dbPath, requestID)
	if !bytes.Contains(storedRequest, []byte(sentinel)) {
		t.Fatalf("stored request body did not contain sentinel")
	}
	if !bytes.Equal(storedResponse, upstreamBody) {
		t.Fatalf("stored response body = %q want %q", storedResponse, upstreamBody)
	}
}
