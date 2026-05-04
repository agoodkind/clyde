package webapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

func newTestServer(t *testing.T, cfg config.WebAppConfig, deps Deps) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, deps, log)
	return httptest.NewServer(s.routes())
}

func TestHealthOK(t *testing.T) {
	ts := newTestServer(t, config.WebAppConfig{}, Deps{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestShutdownClosesIdleKeepaliveConnection(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(config.WebAppConfig{}, Deps{}, log)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.StartOnListener(ctx, lis) }()

	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
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
	if err := s.Shutdown(shutCtx); err != nil {
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

func TestCloseForceClosesActiveConnection(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(config.WebAppConfig{}, Deps{}, log)
	started := make(chan struct{})
	release := make(chan struct{})
	s.mux.HandleFunc("/test/block", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.StartOnListener(ctx, lis) }()

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

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := s.Shutdown(shutCtx); err == nil {
		shutCancel()
		close(release)
		t.Fatalf("shutdown unexpectedly completed while handler was active")
	}
	shutCancel()
	if err := s.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		close(release)
		t.Fatalf("close: %v", err)
	}

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

func TestStartOnListenerServesHealth(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(config.WebAppConfig{}, Deps{}, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.StartOnListener(ctx, lis) }()

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

func TestIndexRendersHTML(t *testing.T) {
	ts := newTestServer(t, config.WebAppConfig{}, Deps{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "clyde remote") {
		t.Fatalf("missing title in body")
	}
}

func TestTokenAuthEnforced(t *testing.T) {
	os.Unsetenv("CLYDE_WEBAPP_TOKEN")
	ts := newTestServer(t, config.WebAppConfig{RequireToken: "secret"}, Deps{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/live-sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/live-sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("with token = %d, want 200", r2.StatusCode)
	}
}

func TestLiveSessionsEndpointSerializesDeps(t *testing.T) {
	ts := newTestServer(t, config.WebAppConfig{}, Deps{
		ListLiveSessions: func(context.Context) ([]LiveSession, error) {
			return []LiveSession{{
				Provider:       "codex",
				SessionName:    "live-demo",
				SessionID:      "codex-123",
				Status:         "running",
				SupportsSend:   true,
				SupportsStream: true,
				SupportsStop:   true,
			}}, nil
		},
	})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/live-sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got liveSessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].SessionID != "codex-123" {
		t.Fatalf("unexpected payload: %+v", got)
	}
}

func TestStartLiveSessionUsesDaemonLaunch(t *testing.T) {
	var got StartLiveSessionRequest
	ts := newTestServer(t, config.WebAppConfig{}, Deps{
		StartLiveSession: func(_ context.Context, req StartLiveSessionRequest) (LiveSession, error) {
			got = req
			return LiveSession{
				Provider:       "codex",
				SessionName:    "chat-demo",
				SessionID:      "codex-demo",
				Status:         "starting",
				SupportsSend:   true,
				SupportsStream: true,
			}, nil
		},
	})
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/live-sessions", "application/json", strings.NewReader(`{"provider":"codex","name":"demo","basedir":"/tmp/demo","model":"gpt","effort":"high","incognito":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if got.Provider != "codex" || got.Name != "demo" || got.Basedir != "/tmp/demo" || got.Model != "gpt" || got.Effort != "high" || !got.Incognito {
		t.Fatalf("launch args = %+v", got)
	}
	var body startLiveSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Session.SessionID != "codex-demo" {
		t.Fatalf("session_id = %q, want codex-demo", body.Session.SessionID)
	}
}

func TestSendLiveSessionUsesDaemonSend(t *testing.T) {
	var gotSessionID, gotText string
	ts := newTestServer(t, config.WebAppConfig{}, Deps{
		SendLiveSession: func(_ context.Context, sessionID, text string) error {
			gotSessionID = sessionID
			gotText = text
			return nil
		},
	})
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/live-sessions/codex-demo/send", "application/json", strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if gotSessionID != "codex-demo" || gotText != "hello" {
		t.Fatalf("send args = (%q, %q)", gotSessionID, gotText)
	}
}

func TestStopLiveSessionUsesDaemonStop(t *testing.T) {
	var gotSessionID string
	ts := newTestServer(t, config.WebAppConfig{}, Deps{
		StopLiveSession: func(_ context.Context, sessionID string) error {
			gotSessionID = sessionID
			return nil
		},
	})
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/live-sessions/codex-demo/stop", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if gotSessionID != "codex-demo" {
		t.Fatalf("stop session = %q, want codex-demo", gotSessionID)
	}
}

func TestStreamLiveSessionWritesSSE(t *testing.T) {
	events := make(chan LiveSessionEvent, 1)
	events <- LiveSessionEvent{SessionID: "codex-demo", Kind: "message", Role: "assistant", Text: "hi"}
	close(events)
	ts := newTestServer(t, config.WebAppConfig{}, Deps{
		StreamLiveSession: func(context.Context, string) (<-chan LiveSessionEvent, error) {
			return events, nil
		},
	})
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/live-sessions/codex-demo/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "event: live-session") || !strings.Contains(text, `"text":"hi"`) {
		t.Fatalf("unexpected stream body: %s", text)
	}
}
