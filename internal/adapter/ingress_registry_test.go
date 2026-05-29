package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// newTestServer constructs a minimal Server for ingress registry tests.
// It uses an in-memory registry with a no-op log so tests run quietly.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := baseConfig()
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		ScratchDir: func() string { return t.TempDir() },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// TestIngressRegistryRegisterAndRelease verifies that a request is
// registered in the ingress registry on handler entry and released on
// handler return. ActiveRequestCount should go from 0 to 1 during
// execution and back to 0 after.
func TestIngressRegistryRegisterAndRelease(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	inFlight := make(chan struct{})
	release := make(chan struct{})

	// Wire through s.handle so the ingress boundary runs.
	handler := srv.handle(adapterRouteOpenAI, func(ctx context.Context, hctx *handlerCtx) error {
		close(inFlight)
		<-release
		writeJSON(hctx.Writer, json.RawMessage(`{"ok":"true"}`))
		return nil
	})

	if count := srv.ActiveRequestCount(); count != 0 {
		t.Fatalf("initial count = %d, want 0", count)
	}

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
	}()

	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not start")
	}

	if count := srv.ActiveRequestCount(); count != 1 {
		t.Errorf("in-flight count = %d, want 1", count)
	}

	close(release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("request did not complete")
	}

	if count := srv.ActiveRequestCount(); count != 0 {
		t.Errorf("post-release count = %d, want 0", count)
	}
}

// TestIngressRegistryDrainRejectsNewRequests verifies that once the
// ingress registry is draining, new requests are rejected with a
// non-2xx error response rather than being dispatched to the handler.
// The exact HTTP status depends on the route family renderer (the
// OpenAI boundary maps upstream errors to HTTP 400); what matters is
// that the handler is not called and the response is an error.
func TestIngressRegistryDrainRejectsNewRequests(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	// Transition the registry to draining state.
	drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	srv.requests.Drain(drainCtx, "test.drain")

	handlerCalled := false
	handler := srv.handle(adapterRouteOpenAI, func(_ context.Context, hctx *handlerCtx) error {
		handlerCalled = true
		writeJSON(hctx.Writer, json.RawMessage(`{"ok":"true"}`))
		return nil
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if handlerCalled {
		t.Fatal("handler should not have been called while registry is draining")
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-2xx when draining", rec.Code)
	}
}

// TestIngressRegistryWaitForIdleReturnZero verifies that WaitForIdle
// returns 0 immediately when no requests are in flight, matching the
// previous WaitForIdle semantics (which polled the conn-state map).
func TestIngressRegistryWaitForIdleReturnZero(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := srv.WaitForIdle(ctx)
	if got != 0 {
		t.Fatalf("WaitForIdle() = %d, want 0 when idle", got)
	}
}

// TestIngressRegistryWaitForIdlePollsCount verifies that WaitForIdle
// blocks while a request is in flight and returns 0 once it completes,
// providing parity with the previous conn-state-map-based semantics.
func TestIngressRegistryWaitForIdlePollsCount(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	inFlight := make(chan struct{})
	release := make(chan struct{})

	handler := srv.handle(adapterRouteOpenAI, func(ctx context.Context, hctx *handlerCtx) error {
		close(inFlight)
		<-release
		writeJSON(hctx.Writer, json.RawMessage(`{"ok":"true"}`))
		return nil
	})

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
	}()

	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not start")
	}

	idleDone := make(chan int)
	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		idleDone <- srv.WaitForIdle(waitCtx)
	}()

	// Release the handler and verify WaitForIdle returns 0.
	close(release)
	select {
	case count := <-idleDone:
		if count != 0 {
			t.Errorf("WaitForIdle returned %d after release, want 0", count)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("WaitForIdle did not return after handler released")
	}

	select {
	case <-reqDone:
	case <-time.After(time.Second):
		t.Fatalf("request goroutine did not exit")
	}
}

// TestIngressRegistryForceCloseOnDeadline verifies that ForceCloseAll
// drains the ingress registry, terminating registered sessions so
// WaitForIdle returns 0 afterwards.
func TestIngressRegistryForceCloseOnDeadline(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	inFlight := make(chan struct{}, 1)
	release := make(chan struct{})

	handler := srv.handle(adapterRouteOpenAI, func(ctx context.Context, hctx *handlerCtx) error {
		inFlight <- struct{}{}
		<-release
		return nil
	})

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
	}()

	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did not start")
	}

	if count := srv.ActiveRequestCount(); count != 1 {
		t.Errorf("pre-force-close count = %d, want 1", count)
	}

	// ForceCloseAll drains the registry; httptest scenarios use a
	// context-cancel closer and still remove the session from the
	// registry so the count drops to 0.
	srv.ForceCloseAll()
	close(release)

	if count := srv.ActiveRequestCount(); count != 0 {
		t.Errorf("post-force-close count = %d, want 0", count)
	}

	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("request did not complete after ForceCloseAll")
	}
}

// TestIngressRegistryConnContextInjectsCloser verifies that when a
// real TCP listener is used, the ConnContext hook injects the net.Conn
// into the request context so the ingress session can be backed by an
// ingressConnCloser rather than a noopIngressCloser.
func TestIngressRegistryConnContextInjectsCloser(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	lis, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blockStarted := make(chan struct{})
	released := make(chan struct{})
	srv.mux.HandleFunc("/test/closer-check", func(w http.ResponseWriter, r *http.Request) {
		// Verify the conn is present in the context via ingressConnKey.
		if conn, ok := r.Context().Value(ingressConnKey{}).(net.Conn); !ok || conn == nil {
			t.Errorf("ingressConnKey not present in request context on real TCP conn")
		}
		close(blockStarted)
		<-released
		w.WriteHeader(http.StatusOK)
	})

	srvDone := make(chan error, 1)
	go func() {
		srvDone <- srv.StartOnListener(ctx, lis)
	}()

	go func() {
		conn, dialErr := net.Dial("tcp", lis.Addr().String())
		if dialErr != nil {
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("GET /test/closer-check HTTP/1.1\r\nHost: localhost\r\n\r\n")); err != nil {
			return
		}
		buf := make([]byte, 64)
		_, _ = conn.Read(buf)
	}()

	select {
	case <-blockStarted:
	case <-time.After(2 * time.Second):
		close(released)
		cancel()
		t.Fatalf("handler did not start")
	}

	close(released)
	cancel()
	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("server exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("server did not stop")
	}
}

// TestIngressMetaIsLivetrackMeta verifies that IngressMeta satisfies
// the livetrack.Meta constraint, which is enforced at compile time via
// the generic type parameter but good to document explicitly.
func TestIngressMetaIsLivetrackMeta(t *testing.T) {
	t.Parallel()
	var _ livetrack.Meta = IngressMeta{}
}

// TestIngressContextCancelCloserClose verifies that the httptest fallback
// closer cancels its context.
func TestIngressContextCancelCloserClose(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	c := &contextCancelCloser{cancel: cancel}
	if err := c.Close("test"); err != nil {
		t.Fatalf("contextCancelCloser.Close returned error: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("contextCancelCloser.Close did not cancel context")
	}
	cancel()
}
