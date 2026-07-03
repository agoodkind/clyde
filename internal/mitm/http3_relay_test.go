package mitm

import (
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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/slogger"
)

func TestProviderHTTP3InterceptsAndCapturesStreamingRequest(t *testing.T) {
	const providerHost = "api2direct.cursor.sh"
	const requestID = "req_cursor_h3_capture_123"
	const sentinel = "CURSOR_H3_SENTINEL_DO_NOT_LOG"

	firstBodyChunk := make(chan struct{})
	releaseResponse := make(chan struct{})
	var firstBodyOnce sync.Once
	var releaseResponseOnce sync.Once
	releaseUpstreamResponse := func() {
		releaseResponseOnce.Do(func() {
			close(releaseResponse)
		})
	}
	upstream := startHTTP3Upstream(t, providerHost, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 3 {
			t.Errorf("upstream protocol = %s, want HTTP/3", r.Proto)
		}
		if r.URL.Path != "/aiserver.v1.AiService/BidiAppend" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, err := io.ReadAll(&notifyReader{
			reader: r.Body,
			notify: func() {
				firstBodyOnce.Do(func() {
					close(firstBodyChunk)
				})
			},
		})
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		decodedBody, _ := decodeForCapture(body, r.Header.Get("content-encoding"))
		if !bytes.Contains(decodedBody, []byte(sentinel)) {
			t.Errorf("upstream body did not contain sentinel")
		}
		<-releaseResponse
		w.Header().Set("content-type", "application/protobuf")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("h3 streamed response"))
	}))
	defer upstream.Close()
	defer releaseUpstreamResponse()

	captureDir := t.TempDir()
	dbPath := filepath.Join(captureDir, "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open capture store: %v", err)
	}
	proxy := startHTTP3MITMTestProxy(t, captureDir, providerHost, upstream, store)
	defer proxy.shutdown()

	bodyReader, bodyWriter := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, "https://"+providerHost+"/aiserver.v1.AiService/BidiAppend", bodyReader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("content-type", "application/protobuf")
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-original-request-id", "orig-h3-123")
	req.Header.Set("x-session-id", "sess-h3-123")
	req.Header.Set("traceparent", "00-1123456789abcdef1123456789abcdef-1123456789abcdef-01")

	client := proxy.http3Client(t, providerHost)
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, roundTripErr := client.RoundTrip(req)
		if roundTripErr != nil {
			errCh <- roundTripErr
			return
		}
		respCh <- resp
	}()

	if _, err := bodyWriter.Write([]byte("prefix " + sentinel)); err != nil {
		t.Fatalf("write request prefix: %v", err)
	}
	select {
	case <-firstBodyChunk:
	case err := <-errCh:
		t.Fatalf("h3 round trip failed before upstream saw body: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive request body before client closed it")
	}
	if _, err := bodyWriter.Write([]byte(" suffix")); err != nil {
		t.Fatalf("write request suffix: %v", err)
	}
	if err := bodyWriter.Close(); err != nil {
		t.Fatalf("close request body: %v", err)
	}
	releaseUpstreamResponse()

	var resp *http.Response
	select {
	case resp = <-respCh:
	case err := <-errCh:
		t.Fatalf("h3 round trip: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for h3 response")
	}
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d want %d", resp.StatusCode, http.StatusAccepted)
	}
	if string(gotBody) != "h3 streamed response" {
		t.Fatalf("response body = %q", gotBody)
	}

	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close capture store: %v", err)
	}
	storedRequest, storedResponse := readStoredCaptureBodies(t, dbPath, requestID)
	if !bytes.Contains(storedRequest, []byte(sentinel)) {
		t.Fatalf("stored request body missing sentinel")
	}
	if string(storedResponse) != "h3 streamed response" {
		t.Fatalf("stored response = %q", storedResponse)
	}
	assertStoredCaptureProtocol(t, dbPath, requestID, "cursor", "cursor.bidi")
}

type notifyReader struct {
	reader io.Reader
	notify func()
}

func (r *notifyReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && r.notify != nil {
		r.notify()
	}
	return n, err
}

type http3Upstream struct {
	server *http3.Server
	conn   net.PacketConn
	addr   string
}

func startHTTP3Upstream(t *testing.T, host string, handler http.Handler) *http3Upstream {
	t.Helper()
	conn, err := net.ListenPacket("udp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen h3 upstream: %v", err)
	}
	caDir := t.TempDir()
	ca, err := loadOrCreateCertAuthority(
		filepath.Join(caDir, "ca.crt"),
		filepath.Join(caDir, "ca.key"),
		time.Now,
	)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("load upstream ca: %v", err)
	}
	leaf, err := ca.leafForHost(host)
	if err != nil {
		_ = conn.Close()
		t.Fatalf("mint upstream leaf: %v", err)
	}
	server := &http3.Server{
		TLSConfig: http3.ConfigureTLSConfig(&tls.Config{
			Certificates: []tls.Certificate{*leaf},
			MinVersion:   tls.VersionTLS13,
		}),
		Handler: handler,
	}
	go func() {
		if serveErr := server.Serve(conn); serveErr != nil && serveErr != http.ErrServerClosed {
			t.Errorf("serve h3 upstream: %v", serveErr)
		}
	}()
	return &http3Upstream{server: server, conn: conn, addr: conn.LocalAddr().String()}
}

func (s *http3Upstream) Close() {
	_ = s.server.Close()
	_ = s.conn.Close()
}

func startHTTP3MITMTestProxy(t *testing.T, captureDir string, providerHost string, upstream *http3Upstream, store *capture.Store) *testProxy {
	t.Helper()
	RegisterProviderFirst(testCursorProvider{host: providerHost})
	t.Cleanup(func() {
		UnregisterProvider(ProviderIDCursor)
	})
	tcpListener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen tcp proxy: %v", err)
	}
	tcpAddr, ok := tcpListener.Addr().(*net.TCPAddr)
	if !ok {
		_ = tcpListener.Close()
		t.Fatalf("tcp listener addr is %T, want *net.TCPAddr", tcpListener.Addr())
	}
	udpConn, err := net.ListenPacket("udp", net.JoinHostPort("::1", fmt.Sprint(tcpAddr.Port)))
	if err != nil {
		_ = tcpListener.Close()
		t.Fatalf("listen udp proxy: %v", err)
	}
	caDir := t.TempDir()
	mitmCfg := config.MITMConfig{
		CA: config.MITMCAConfig{
			CertPath: filepath.Join(caDir, "ca.crt"),
			KeyPath:  filepath.Join(caDir, "ca.key"),
		},
		CaptureDir: captureDir,
	}
	logger := slog.New(newMITMCaptureTestHandler(t, captureDir))
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	proxy, err := NewProxy(mitmCfg, config.LoggingRequest{}, logger, []net.Listener{tcpListener}, []net.PacketConn{udpConn}, store, "test", group)
	if err != nil {
		_ = udpConn.Close()
		_ = tcpListener.Close()
		t.Fatalf("NewProxy: %v", err)
	}
	proxy.tlsClientConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{http3.NextProtoH3}}
	proxy.h3ResolveUDPAddr = func(context.Context, string) (net.Addr, error) {
		return net.ResolveUDPAddr("udp", upstream.addr)
	}
	proxy.requestLog = logevent.NewEmitter(slogger.WithConcern(logger, slogger.ConcernProviderMITMWire), nil)
	server := &http.Server{Handler: http.HandlerFunc(proxy.handle)}
	proxy.server = server
	go func() { _ = server.Serve(tcpListener) }()
	go func() { _ = proxy.ServeQUIC(context.Background()) }()
	return &testProxy{server: server, proxy: proxy, group: group, addr: tcpListener.Addr().String()}
}

func (t *testProxy) http3Client(tb testing.TB, host string) *http3.Transport {
	tb.Helper()
	caPool := x509.NewCertPool()
	caPool.AddCert(t.proxy.ca.cert)
	proxyAddr := t.addr
	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		tb.Fatalf("listen h3 client udp: %v", err)
	}
	quicTransport := &quic.Transport{Conn: clientConn}
	tb.Cleanup(func() {
		_ = quicTransport.Close()
		_ = clientConn.Close()
	})
	transport := &http3.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    caPool,
			ServerName: host,
			MinVersion: tls.VersionTLS13,
		},
		Dial: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			udpAddr, resolveErr := net.ResolveUDPAddr("udp", proxyAddr)
			if resolveErr != nil {
				return nil, fmt.Errorf("resolve proxy udp addr: %w", resolveErr)
			}
			return quicTransport.DialEarly(ctx, udpAddr, tlsCfg, cfg)
		},
		DisableCompression: true,
	}
	tb.Cleanup(func() {
		_ = transport.Close()
	})
	return transport
}

func assertStoredCaptureProtocol(t *testing.T, dbPath string, requestID string, provider string, concern string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var gotProvider string
	var gotConcern string
	row := db.QueryRow(`SELECT provider, concern FROM requests WHERE request_id=? ORDER BY ts DESC LIMIT 1`, requestID)
	if err := row.Scan(&gotProvider, &gotConcern); err != nil {
		t.Fatalf("scan request row: %v", err)
	}
	if gotProvider != provider {
		t.Fatalf("provider = %q want %q", gotProvider, provider)
	}
	if gotConcern != concern {
		t.Fatalf("concern = %q want %q", gotConcern, concern)
	}
}
