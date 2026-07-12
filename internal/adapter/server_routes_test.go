package adapter

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

func TestStartOnListenerServesHealth(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := baseConfig()
	cfg.Enabled = true
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.StartOnListener(ctx, lis) }()

	resp, err := http.Get("http://" + lis.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not stop")
	}
}

func TestShutdownClosesIdleKeepaliveConnection(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := baseConfig()
	cfg.Enabled = true
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- srv.StartOnListener(ctx, lis) }()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", lis.Addr().String()); err != nil {
		t.Fatalf("write first request: %v", err)
	}
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read first response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d, want 200", resp.StatusCode)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), time.Second)
	if err := srv.ShutdownHTTP(shutCtx); err != nil {
		shutCancel()
		t.Fatalf("shutdown: %v", err)
	}
	shutCancel()

	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", lis.Addr().String()); err == nil {
		if resp, err := http.ReadResponse(reader, nil); err == nil {
			_ = resp.Body.Close()
			t.Fatalf("idle keepalive connection served request after shutdown with status %d", resp.StatusCode)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not stop")
	}
}

// TestCloseForceClosesActiveConnection verifies that the lifecycle group's
// Quiesce force-closes an active ingress connection at the budget cap: the
// ingress session's closer terminates the underlying net.Conn, which cancels
// the handler's request context and lets the HTTP server shut down. This is the
// path that replaced the standalone Shutdown/ForceCloseAll/Close methods.
func TestCloseForceClosesActiveConnection(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg := baseConfig()
	cfg.Enabled = true
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{Group: group}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// Register the adapter HTTP-server shutdown as the daemon does, so Quiesce
	// stops the server once the force-closed handler returns.
	group.AddHookBefore(livetrack.PhaseEgress, "adapter.http_shutdown", func(ctx context.Context) error {
		return srv.ShutdownHTTP(ctx)
	})
	started := make(chan struct{})
	release := make(chan struct{})
	// Route through the ingress boundary (s.handle) so the request registers an
	// ingress session backed by an ingressConnCloser over the real net.Conn.
	// Quiesce force-closes that session, closing the connection.
	srv.mux.HandleFunc("/test/block", srv.handle(adapterRouteOpenAI, func(ctx context.Context, hctx *handlerCtx) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}))
	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- srv.StartOnListener(ctx, lis) }()

	client := &http.Client{}
	respCh := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + lis.Addr().String() + "/test/block")
		if resp != nil {
			_ = resp.Body.Close()
		}
		respCh <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("handler did not start")
	}

	// Quiesce waits the short cap then force-closes the active ingress session,
	// closing the connection and unblocking the handler and HTTP server.
	group.Quiesce(context.Background(), "test.close", livetrack.Budget{Cap: 200 * time.Millisecond, IdleGrace: 0})

	select {
	case <-respCh:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("active client was not closed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit: %v", err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatalf("server did not stop")
	}
}

func TestAnthropicMessagesRouteUsesNativeIngress(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = true
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		ExecutePrepared: func(_ context.Context, req anthropic.PreparedRequest, writer adapterprovider.EventWriter) (adapterprovider.Result, error) {
			if !req.NativeIngress {
				t.Fatalf("NativeIngress = false, want true")
			}
			if got := req.Request.Model; got != "claude-haiku-4-5-20251001" {
				t.Fatalf("prepared model = %q", got)
			}
			if len(req.Request.Messages) != 1 || len(req.Request.Messages[0].Content) != 1 {
				t.Fatalf("prepared messages = %+v", req.Request.Messages)
			}
			nativeWriter, ok := writer.(*nativeAnthropicJSONWriter)
			if !ok {
				t.Fatalf("writer type = %T, want *nativeAnthropicJSONWriter", writer)
			}
			body := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-haiku-4-5-20251001","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
			if err := nativeWriter.capture(http.Header{"Content-Type": {"application/json"}}, body); err != nil {
				t.Fatalf("capture: %v", err)
			}
			return adapterprovider.Result{}, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"clyde-haiku-4-5","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"message"`) {
		t.Fatalf("body missing anthropic message envelope: %s", body)
	}
	if strings.Contains(body, `"chat.completion"`) {
		t.Fatalf("body unexpectedly contains OpenAI envelope: %s", body)
	}
}

func TestAnthropicMessagesRoutePreservesNativeClaudeModelID(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = true
	cfg.OpenAICompatPassthrough = config.AdapterOpenAICompatPassthrough{
		BaseURL: "http://localhost:1234",
		Model:   "local-model",
	}
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		ExecutePrepared: func(_ context.Context, req anthropic.PreparedRequest, writer adapterprovider.EventWriter) (adapterprovider.Result, error) {
			if !req.NativeIngress {
				t.Fatalf("NativeIngress = false, want true")
			}
			if got := req.Request.Model; got != "claude-opus-4-7" {
				t.Fatalf("prepared model = %q", got)
			}
			nativeWriter, ok := writer.(*nativeAnthropicJSONWriter)
			if !ok {
				t.Fatalf("writer type = %T, want *nativeAnthropicJSONWriter", writer)
			}
			body := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-opus-4-7","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
			if err := nativeWriter.capture(http.Header{"Content-Type": {"application/json"}}, body); err != nil {
				t.Fatalf("capture: %v", err)
			}
			return adapterprovider.Result{}, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAnthropicMessagesRoutePreservesSSEFramesWithClaudeBetaQuery(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = true
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	srv.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{}, anthropic.ProviderOptions{
		ExecutePrepared: func(_ context.Context, req anthropic.PreparedRequest, writer adapterprovider.EventWriter) (adapterprovider.Result, error) {
			if !req.NativeIngress || !req.Request.Stream {
				t.Fatalf("prepared ingress=%v stream=%v, want native stream", req.NativeIngress, req.Request.Stream)
			}
			nativeWriter, ok := writer.(*nativeAnthropicStreamWriter)
			if !ok {
				t.Fatalf("writer type = %T, want *nativeAnthropicStreamWriter", writer)
			}
			nativeWriter.commit(http.Header{"Content-Type": {"text/event-stream"}})
			if err := nativeWriter.write([]byte("event: message_start\n")); err != nil {
				t.Fatalf("write event: %v", err)
			}
			if err := nativeWriter.write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_123\"}}\n\n")); err != nil {
				t.Fatalf("write data: %v", err)
			}
			return adapterprovider.Result{}, nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(`{"model":"clyde-haiku-4-5","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"stream":true}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Fatalf("body missing anthropic SSE event: %s", body)
	}
	if strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("body unexpectedly contains OpenAI chunk framing: %s", body)
	}
}

func TestAnthropicCountTokensRouteReturnsRealCount(t *testing.T) {
	var gotKey, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":123}`))
	}))
	defer upstream.Close()

	cfg := baseConfig()
	cfg.Enabled = true
	cfg.Anthropic.OAuth.MessagesURL = upstream.URL + "/v1/messages"
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Setenv("CLYDE_ANTHROPIC_API_KEY", "test-key")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens":123`) {
		t.Fatalf("body = %s, want input_tokens 123", rec.Body.String())
	}
	if gotKey != "test-key" {
		t.Errorf("upstream x-api-key = %q, want %q", gotKey, "test-key")
	}
	if gotAuth != "" {
		t.Errorf("upstream Authorization = %q, want empty (OAuth must not be used)", gotAuth)
	}
}

func TestAnthropicCountTokensRouteRequiresKey(t *testing.T) {
	cfg := baseConfig()
	cfg.Enabled = true
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Setenv("CLYDE_ANTHROPIC_API_KEY", "")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotImplemented {
		t.Fatalf("status = 501, want a typed key-required error, not the old stub")
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want an error when no API key is configured")
	}
	if !strings.Contains(rec.Body.String(), "CLYDE_ANTHROPIC_API_KEY") {
		t.Fatalf("body = %s, want a message naming the required key env var", rec.Body.String())
	}
}
