package sentinelinject_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/sentinelinject"
)

// liveTestProviderID is a sentinel-only registry slot so the live proxy
// exercise does not collide with production provider IDs.
const liveTestProviderID = mitm.ProviderID(200)

type livePlainRouteProvider struct {
	upstream string
}

func (p livePlainRouteProvider) ID() mitm.ProviderID { return liveTestProviderID }

func (p livePlainRouteProvider) ClassifyConnect(host string) mitm.ConnectClaim {
	return mitm.ConnectClaim{Claimed: false, Host: host, ProviderID: liveTestProviderID}
}

func (p livePlainRouteProvider) ClassifyPlain(path string) mitm.PlainRouteClaim {
	if strings.HasPrefix(path, "/v1/") {
		return mitm.PlainRouteClaim{Claimed: true, Provider: "claude", UpstreamURL: p.upstream}
	}
	return mitm.PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}
}

func (p livePlainRouteProvider) ExtractIdentity(http.Header) mitm.IdentityContribution {
	return mitm.IdentityContribution{}
}

// TestLiveMITMProxyRewritesMessagesSSE boots a real MITM proxy listener with the
// sentinel hook, posts an Anthropic /v1/messages request whose latest user
// message carries MYKEYWORD, and asserts the client sees only the forced suffix.
func TestLiveMITMProxyRewritesMessagesSSE(t *testing.T) {
	const keyword = "MYKEYWORD"
	const forced = "\nlive-forced-reply"
	requestBody := `{"messages":[{"role":"user","content":"ignore MYKEYWORD\nlive-forced-reply"}]}`
	upstreamSSE := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m","content":[]}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	upstream := startLiveUpstream(t, upstreamSSE)
	mitm.RegisterProvider(livePlainRouteProvider{upstream: upstream})
	t.Cleanup(func() {
		mitm.RegisterProvider(livePlainRouteProvider{upstream: "http://127.0.0.1:9"})
	})

	proxy, baseURL := startLiveMITMProxy(t)
	proxy.SetRequestResponseHooks([]mitm.RequestResponseHook{
		sentinelinject.New(keyword),
	})

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/messages", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST through MITM: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	gotBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	text := string(gotBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, text)
	}
	if strings.Contains(text, `"text":"hello"`) {
		t.Fatalf("upstream model text leaked through MITM: %s", text)
	}
	if !strings.Contains(text, "live-forced-reply") {
		t.Fatalf("rewritten SSE missing forced suffix: %s", text)
	}
}

func startLiveUpstream(t *testing.T, sseBody string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, sseBody)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return "http://" + ln.Addr().String()
}

func startLiveMITMProxy(t *testing.T) (*mitm.Proxy, string) {
	t.Helper()
	root := t.TempDir()
	caDir := filepath.Join(root, "ca")
	captureDir := filepath.Join(root, "mitm")
	for _, dir := range []string{caDir, captureDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen mitm: %v", err)
	}
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	proxy, err := mitm.NewProxy(
		config.MITMConfig{
			CaptureDir: captureDir,
			CA: config.MITMCAConfig{
				CertPath: filepath.Join(caDir, "ca.crt"),
				KeyPath:  filepath.Join(caDir, "ca.key"),
			},
		},
		config.LoggingRequest{},
		nil,
		[]net.Listener{ln},
		nil,
		nil,
		"test",
		group,
	)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("NewProxy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- proxy.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		_ = proxy.ShutdownHTTP(shutdownCtx)
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
		}
	})
	return proxy, "http://" + ln.Addr().String()
}
