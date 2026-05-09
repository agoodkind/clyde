package webapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// TestSSERegistersOnOpenAndReleasesOnClose verifies that opening an
// SSE stream increments Channels.Count and canceling the request
// context brings it back to zero.
func TestSSERegistersOnOpenAndReleasesOnClose(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// eventsCh stays open so the SSE handler blocks in the select loop.
	eventsCh := make(chan LiveSessionEvent)
	s := New(config.WebAppConfig{}, Deps{
		StreamLiveSession: func(_ context.Context, _ string) (<-chan LiveSessionEvent, error) {
			return eventsCh, nil
		},
	}, log)
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	srvDone := make(chan error, 1)
	go func() { srvDone <- s.StartOnListener(srvCtx, lis) }()

	// Build the SSE request with its own cancel so we can simulate
	// client disconnect without relying on TCP keepalive behavior.
	reqCtx, reqCancel := context.WithCancel(context.Background())
	req, reqErr := http.NewRequestWithContext(reqCtx,
		http.MethodGet,
		"http://"+lis.Addr().String()+"/api/live-sessions/demo-session/stream",
		nil)
	if reqErr != nil {
		reqCancel()
		t.Fatalf("build request: %v", reqErr)
	}
	clientCh := make(chan error, 1)
	go func() {
		resp, doErr := (&http.Client{}).Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		clientCh <- doErr
	}()

	// Poll for the session to appear in the registry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Channels.Count() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.Channels.Count() != 1 {
		reqCancel()
		t.Fatalf("Channels.Count after open = %d, want 1", s.Channels.Count())
	}

	// Cancel the request context to simulate the client disconnecting.
	reqCancel()

	// Poll for the session to be released.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Channels.Count() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.Channels.Count() != 0 {
		t.Fatalf("Channels.Count after disconnect = %d, want 0", s.Channels.Count())
	}

	srvCancel()
	select {
	case serverErr := <-srvDone:
		if serverErr != nil {
			t.Fatalf("server exit: %v", serverErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

// TestCloseForceClosesRegisteredSSESessions verifies that Close drains
// the Channels registry, invoking force-close on any open SSE stream
// so the client goroutine exits.
func TestCloseForceClosesRegisteredSSESessions(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// blockCh keeps the SSE handler alive until we close the server.
	streamStarted := make(chan struct{})
	s := New(config.WebAppConfig{}, Deps{
		StreamLiveSession: func(_ context.Context, _ string) (<-chan LiveSessionEvent, error) {
			// Signal that the stream dep was invoked. The channel is
			// never closed or written; the handler blocks in the select
			// loop until streamCtx is canceled by force-close.
			select {
			case <-streamStarted:
			default:
				close(streamStarted)
			}
			return make(chan LiveSessionEvent), nil
		},
	}, log)
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	srvDone := make(chan error, 1)
	go func() { srvDone <- s.StartOnListener(srvCtx, lis) }()

	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()
	req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodGet,
		"http://"+lis.Addr().String()+"/api/live-sessions/demo-session/stream", nil)
	if reqErr != nil {
		t.Fatalf("build request: %v", reqErr)
	}
	clientCh := make(chan error, 1)
	go func() {
		resp, doErr := (&http.Client{}).Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		clientCh <- doErr
	}()

	select {
	case <-streamStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stream dep was not called")
	}

	// Poll for the channel to register.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Channels.Count() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.Channels.Count() != 1 {
		t.Fatalf("Channels.Count before Close = %d, want 1", s.Channels.Count())
	}

	// Close force-closes all connections including the open SSE stream.
	if closeErr := s.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) && !errors.Is(closeErr, net.ErrClosed) {
		t.Fatalf("Close: %v", closeErr)
	}

	// The client goroutine must unblock because the connection was
	// force-closed by the server.
	select {
	case <-clientCh:
	case <-time.After(3 * time.Second):
		t.Fatal("client was not unblocked after Close")
	}

	srvCancel()
	select {
	case serverErr := <-srvDone:
		if serverErr != nil {
			t.Fatalf("server exit: %v", serverErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

// TestChannelsDrainOnDeadlineForceClosesSSE verifies that a Channels
// drain called with an already-expired context force-closes the open
// SSE session within the registry's CloserGrace deadline.
func TestChannelsDrainOnDeadlineForceClosesSSE(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(config.WebAppConfig{}, Deps{}, log)

	// Manually register a session with a cancel-based closer to mirror
	// what handleStreamLiveSession does, without needing a full HTTP
	// server. This directly exercises the registry drain path.
	cancelCalled := make(chan struct{})
	cancelFn := func() { close(cancelCalled) }
	sess, registerErr := s.Channels.Register(
		context.Background(),
		"sse",
		WebMeta{
			ChannelKind: "sse",
			ClientID:    "test-session",
			RemoteAddr:  "[::1]:0",
			Subscribed:  []string{"test-session"},
		},
		sseCloser{cancel: cancelFn},
	)
	if registerErr != nil {
		t.Fatalf("Register: %v", registerErr)
	}
	if s.Channels.Count() != 1 {
		s.Channels.Release(context.Background(), sess, "cleanup")
		t.Fatalf("Count after Register = %d, want 1", s.Channels.Count())
	}

	// Drain with an already-expired context to trigger force-close.
	expiredCtx, expiredCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expiredCancel()
	result := s.Channels.Drain(expiredCtx, "test.drain")
	if result.ForceClosed != 1 {
		t.Fatalf("ForceClosed = %d, want 1", result.ForceClosed)
	}
	select {
	case <-cancelCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("closer was not called within timeout")
	}
	if s.Channels.State() != livetrack.StateClosed {
		t.Fatalf("State = %s, want closed", s.Channels.State())
	}
}

// TestRegistryRejectsAfterDrain verifies that once the server is
// draining, new SSE stream registrations return 503 with a
// "draining" message.
func TestRegistryRejectsAfterDrain(t *testing.T) {
	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(config.WebAppConfig{}, Deps{
		StreamLiveSession: func(_ context.Context, _ string) (<-chan LiveSessionEvent, error) {
			return make(chan LiveSessionEvent), nil
		},
	}, log)

	// Begin draining so new Register calls are rejected. An already-
	// expired deadline ensures the registry transitions immediately.
	drainCtx, drainCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	drainCancel()
	drainResult := s.Channels.Drain(drainCtx, "test.pre-drain")
	if drainResult.Final != livetrack.StateClosed {
		t.Fatalf("pre-drain final = %s, want closed", drainResult.Final)
	}

	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	srvDone := make(chan error, 1)
	go func() { srvDone <- s.StartOnListener(srvCtx, lis) }()

	resp, clientErr := http.Get("http://" + lis.Addr().String() + "/api/live-sessions/demo-session/stream")
	if clientErr != nil {
		t.Fatalf("GET stream: %v", clientErr)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "draining") {
		t.Fatalf("expected draining message in body, got: %s", string(body))
	}

	srvCancel()
	select {
	case serverErr := <-srvDone:
		if serverErr != nil {
			t.Fatalf("server exit: %v", serverErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}
