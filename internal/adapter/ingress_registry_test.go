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

// newTestServer constructs a minimal Server for ingress registry tests, wired
// to a lifecycle group the test controls. It returns the group so tests can
// drive draining through group.Quiesce, the only public drain entry point.
func newTestServer(t *testing.T) (*Server, *livetrack.Group) {
	t.Helper()
	cfg := baseConfig()
	group := livetrack.NewGroup(livetrack.GroupOptions{Log: nil})
	srv, err := New(context.Background(), cfg, config.LoggingConfig{}, Deps{
		ScratchDir: func() string { return t.TempDir() },
		Group:      group,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, group
}

// TestIngressRegistryRegisterAndRelease verifies that a request is
// registered in the ingress registry on handler entry and released on
// handler return. ActiveRequestCount should go from 0 to 1 during
// execution and back to 0 after.
func TestIngressRegistryRegisterAndRelease(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	inFlight := make(chan struct{})
	release := make(chan struct{})

	// Wire through s.handle so the ingress boundary runs.
	handler := srv.handle(adapterRouteOpenAI, func(ctx context.Context, hctx *handlerCtx) error {
		close(inFlight)
		<-release
		writeJSON(hctx.Writer, json.RawMessage(`{"ok":"true"}`))
		return nil
	})

	if count := srv.requests.Count(); count != 0 {
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

	if count := srv.requests.Count(); count != 1 {
		t.Errorf("in-flight count = %d, want 1", count)
	}

	close(release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("request did not complete")
	}

	if count := srv.requests.Count(); count != 0 {
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
	srv, group := newTestServer(t)

	// Transition the registry to draining/closed through the group, the only
	// public drain entry point.
	drainCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	group.Quiesce(drainCtx, "test.drain", livetrack.Budget{Cap: 50 * time.Millisecond, IdleGrace: 0})

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

// TestIngressRegistryForceCloseOnDeadline verifies that the lifecycle group's
// Quiesce force-closes an in-flight ingress session at the budget cap,
// terminating it so the registry count drops to 0. The wait-for-idle polling
// and force-close fan-out themselves are unit-tested in livetrack.
func TestIngressRegistryForceCloseOnDeadline(t *testing.T) {
	t.Parallel()
	srv, group := newTestServer(t)

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

	if count := srv.requests.Count(); count != 1 {
		t.Errorf("pre-force-close count = %d, want 1", count)
	}

	// Quiesce waits the short cap then force-closes the still-active session
	// (the closer cancels the handler context) and removes it from the
	// registry, so the count drops to 0.
	group.Quiesce(context.Background(), "test.force_close", livetrack.Budget{Cap: 200 * time.Millisecond, IdleGrace: 0})
	close(release)

	if count := srv.requests.Count(); count != 0 {
		t.Errorf("post-force-close count = %d, want 0", count)
	}

	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("request did not complete after force-close")
	}
}

// TestIngressRegistryConnContextInjectsCloser verifies that when a
// real TCP listener is used, the ConnContext hook injects the net.Conn
// into the request context so the ingress session can be backed by an
// ingressConnCloser rather than a noopIngressCloser.
func TestIngressRegistryConnContextInjectsCloser(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

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
