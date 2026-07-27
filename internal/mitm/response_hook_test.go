package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
)

func TestRequestResponseHookAppendsPlainHTTPResponseBody(t *testing.T) {
	const suffix = "\ndata: plain hook\n\n"
	requestBody := []byte(`{"probe":"hook-match"}`)
	upstreamBody := []byte("plain upstream\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if !bytes.Equal(body, requestBody) {
			t.Errorf("upstream body = %q want %q", body, requestBody)
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write(upstreamBody)
	}))
	defer upstream.Close()

	captureDir := t.TempDir()
	dbPath := filepath.Join(captureDir, "capture.db")
	store := openHookTestCaptureStore(t, dbPath)
	proxy := newHTTPProxyForCaptureTest(t, captureDir, store, upstream)
	proxy.SetRequestResponseHooks([]RequestResponseHook{
		testAppendResponseHook{
			provider:     "openai",
			path:         "/v1/responses",
			requiredBody: []byte("hook-match"),
			suffix:       []byte(suffix),
		},
	})

	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	proxy.handle(recorder, req)

	response := recorder.Result()
	defer response.Body.Close()
	gotBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	wantBody := append(append([]byte(nil), upstreamBody...), []byte(suffix)...)
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("response body = %q want %q", gotBody, wantBody)
	}
	if got := response.Header.Get("x-clyde-test-hook"); got != "append" {
		t.Fatalf("x-clyde-test-hook = %q want append", got)
	}

	waitForCaptureRecordWithLeg(t, mitmWireConcernTestPath(captureDir), logevent.LegMITMCaptureIndex)
	closeHookTestCaptureStore(t, store)
	storedResponse := latestStoredResponseBody(t, dbPath)
	if !bytes.Equal(storedResponse, wantBody) {
		t.Fatalf("stored response body = %q want %q", storedResponse, wantBody)
	}
}

func TestRequestResponseHookAppendsProviderTLSResponseBody(t *testing.T) {
	const providerHost = "api2direct.cursor.sh"
	const suffix = "\ndata: provider hook\n\n"
	requestBody := []byte(`{"probe":"hook-match"}`)
	upstreamBody := []byte("provider upstream\n")
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if !bytes.Equal(body, requestBody) {
			t.Errorf("upstream body = %q want %q", body, requestBody)
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write(upstreamBody)
	}))
	defer upstream.Close()

	captureDir := t.TempDir()
	dbPath := filepath.Join(captureDir, "capture.db")
	store := openHookTestCaptureStore(t, dbPath)
	proxy := startCursorMITMTestProxy(t, captureDir, providerHost, upstream, store)
	defer proxy.shutdown()
	proxy.proxy.SetRequestResponseHooks([]RequestResponseHook{
		testAppendResponseHook{
			provider:     "cursor",
			path:         "/aiserver.v1.AiService/BidiAppend",
			requiredBody: []byte("hook-match"),
			suffix:       []byte(suffix),
		},
	})

	tlsClient, tlsReader := connectProviderHTTP1Client(t, proxy, providerHost)
	defer tlsClient.Close()
	req, err := http.NewRequest(http.MethodPost, "https://"+providerHost+"/aiserver.v1.AiService/BidiAppend", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	if err := req.Write(tlsClient); err != nil {
		t.Fatalf("write provider request: %v", err)
	}
	response, err := http.ReadResponse(tlsReader, req)
	if err != nil {
		t.Fatalf("read provider response: %v", err)
	}
	defer response.Body.Close()
	gotBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read provider response body: %v", err)
	}
	wantBody := append(append([]byte(nil), upstreamBody...), []byte(suffix)...)
	if !bytes.Equal(gotBody, wantBody) {
		t.Fatalf("provider response body = %q want %q", gotBody, wantBody)
	}
	if got := response.Header.Get("x-clyde-test-hook"); got != "append" {
		t.Fatalf("provider x-clyde-test-hook = %q want append", got)
	}

	waitForCaptureRecordWithLeg(t, mitmWireConcernTestPath(captureDir), logevent.LegMITMCaptureIndex)
	// The intercepted-TLS path emits its wire legs and only afterwards hands
	// the exchange to the capture store, whose writer is asynchronous, so the
	// capture-index leg does not mean the row exists yet. Closing the store
	// first would drop a record still on its way in and leave nothing to scan.
	waitForStoredResponseBody(t, dbPath)
	closeHookTestCaptureStore(t, store)
	storedResponse := latestStoredResponseBody(t, dbPath)
	if !bytes.Equal(storedResponse, wantBody) {
		t.Fatalf("stored provider response body = %q want %q", storedResponse, wantBody)
	}
}

func TestRequestResponseHookNoHookLeavesPlainHTTPResponseUnchanged(t *testing.T) {
	requestBody := []byte(`{"probe":"no-hook"}`)
	upstreamBody := []byte("plain upstream without hook\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		if !bytes.Equal(body, requestBody) {
			t.Errorf("upstream body = %q want %q", body, requestBody)
		}
		w.Header().Set("content-type", "text/plain")
		_, _ = w.Write(upstreamBody)
	}))
	defer upstream.Close()

	proxy := newHTTPProxyForCaptureTest(t, t.TempDir(), nil, upstream)
	req := httptest.NewRequest(http.MethodPost, "http://clyde.test/v1/responses", bytes.NewReader(requestBody))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	proxy.handle(recorder, req)

	response := recorder.Result()
	defer response.Body.Close()
	gotBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if !bytes.Equal(gotBody, upstreamBody) {
		t.Fatalf("response body = %q want %q", gotBody, upstreamBody)
	}
	if got := response.Header.Get("x-clyde-test-hook"); got != "" {
		t.Fatalf("unexpected hook header %q", got)
	}
}

type testAppendResponseHook struct {
	provider     string
	path         string
	requiredBody []byte
	suffix       []byte
}

func (h testAppendResponseHook) MatchRequestResponse(request RequestResponseHookRequest) (RequestResponseHookMatch, error) {
	if request.Provider != h.provider {
		return RequestResponseHookMatch{}, nil
	}
	if request.Path != h.path {
		return RequestResponseHookMatch{}, nil
	}
	body, err := request.Body.Bytes()
	if err != nil {
		return RequestResponseHookMatch{}, err
	}
	if !bytes.Contains(body, h.requiredBody) {
		return RequestResponseHookMatch{}, nil
	}
	return RequestResponseHookMatch{
		Matched:     true,
		Transformer: testAppendResponseTransformer{suffix: append([]byte(nil), h.suffix...)},
	}, nil
}

type testAppendResponseTransformer struct {
	suffix []byte
}

func (t testAppendResponseTransformer) TransformResponse(_ context.Context, response ResponseHookResponse) (ResponseHookResponse, error) {
	response.Header = response.Header.Clone()
	response.Header.Set("x-clyde-test-hook", "append")
	response.Header.Del("content-length")
	response.ContentLength = -1
	response.Body = io.MultiReader(response.Body, bytes.NewReader(t.suffix))
	return response, nil
}

func connectProviderHTTP1Client(t *testing.T, proxy *testProxy, providerHost string) (*tls.Conn, *bufio.Reader) {
	t.Helper()
	caPool := x509.NewCertPool()
	caPool.AddCert(proxy.proxy.ca.cert)
	client, err := net.DialTimeout("tcp", proxy.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := client.Write([]byte("CONNECT " + providerHost + ":443 HTTP/1.1\r\nHost: " + providerHost + ":443\r\n\r\n")); err != nil {
		_ = client.Close()
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(client)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		_ = client.Close()
		t.Fatalf("read CONNECT status: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		_ = client.Close()
		t.Fatalf("CONNECT status = %q", strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = client.Close()
			t.Fatalf("read CONNECT header: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	tlsClient := tls.Client(&bufferedConn{Conn: client, reader: reader}, &tls.Config{
		ServerName: providerHost,
		RootCAs:    caPool,
		NextProtos: []string{"http/1.1"},
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		_ = tlsClient.Close()
		t.Fatalf("client TLS handshake: %v", err)
	}
	return tlsClient, bufio.NewReader(tlsClient)
}

func openHookTestCaptureStore(t *testing.T, dbPath string) *capture.Store {
	t.Helper()
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	return store
}

func closeHookTestCaptureStore(t *testing.T, store *capture.Store) {
	t.Helper()
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
}

// waitForStoredResponseBody blocks until the capture store has
// committed at least one response body row.
func waitForStoredResponseBody(t *testing.T, dbPath string) {
	t.Helper()
	waitForCaptureCommit(t, dbPath, "stored response body", func(db *sql.DB) (bool, error) {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM bodies WHERE which='response'`).Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	})
}

func latestStoredResponseBody(t *testing.T, dbPath string) []byte {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var storedResponse []byte
	row := db.QueryRow(`SELECT data FROM bodies WHERE which='response' ORDER BY request_row_id DESC LIMIT 1`)
	if err := row.Scan(&storedResponse); err != nil {
		t.Fatalf("scan response body: %v", err)
	}
	return storedResponse
}
