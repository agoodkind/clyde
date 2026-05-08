package ui

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/session"
)

// TestWorkerMeta_IsLivetrackMeta confirms that WorkerMeta satisfies
// the livetrack.Meta constraint at compile time. The test does not
// need to run code; the type assertion in the variable declaration is
// the check.
func TestWorkerMeta_IsLivetrackMeta(t *testing.T) {
	var _ livetrack.Meta = WorkerMeta{}
}

// TestWorkerCloser_CloseIdempotent confirms that calling Close more
// than once on a workerCloser does not panic and fires the cancel
// exactly once.
func TestWorkerCloser_CloseIdempotent(t *testing.T) {
	fired := 0
	cancel := func() { fired++ }
	c := newWorkerCloser(cancel)
	if err := c.Close("first"); err != nil {
		t.Fatalf("first Close returned unexpected err: %v", err)
	}
	if err := c.Close("second"); err != nil {
		t.Fatalf("second Close returned unexpected err: %v", err)
	}
	if fired != 1 {
		t.Errorf("cancel fired %d times, want 1", fired)
	}
}

// TestNoopCloser_Close confirms that noopCloser.Close returns nil and
// does not panic when called multiple times.
func TestNoopCloser_Close(t *testing.T) {
	c := noopCloser{}
	if err := c.Close("any"); err != nil {
		t.Fatalf("noopCloser.Close returned unexpected err: %v", err)
	}
	if err := c.Close("any"); err != nil {
		t.Fatalf("noopCloser.Close second call returned unexpected err: %v", err)
	}
}

// TestStartRunSupervisor_RegisterReleasePairing confirms that a
// supervisor goroutine launched via startRunSupervisor registers
// itself with the workers registry and releases itself when the stop
// channel is closed.
func TestStartRunSupervisor_RegisterReleasePairing(t *testing.T) {
	a := NewApp(nil, AppCallbacks{})

	started := make(chan struct{})
	exited := make(chan struct{})

	stop := a.startRunSupervisor("test_supervisor", func(stop <-chan struct{}) {
		close(started)
		<-stop
		close(exited)
	})

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor goroutine did not start")
	}

	if count := a.workers.Count(); count != 1 {
		t.Errorf("workers.Count() = %d, want 1 after supervisor started", count)
	}

	close(stop)

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor goroutine did not exit after stop channel closed")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.workers.Count() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if count := a.workers.Count(); count != 0 {
		t.Errorf("workers.Count() = %d, want 0 after stop channel closed", count)
	}
}

// TestLoadDetailAsync_RegisterReleasePairing confirms that a detail-
// load goroutine registers itself with the workers registry and
// releases itself when the callback returns.
func TestLoadDetailAsync_RegisterReleasePairing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess := &session.Session{
		Name:     "register-test",
		Metadata: session.Metadata{Name: "register-test"},
	}

	callbackDone := make(chan struct{})
	cb := AppCallbacks{
		GetSessionDetail: func(_ context.Context, _ *session.Session) (SessionDetail, error) {
			<-callbackDone
			return SessionDetail{}, nil
		},
	}

	a := NewApp([]*session.Session{sess}, cb, AppOptions{Context: ctx})
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)
	a.screen = scr

	a.loadDetailAsync(sess)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.workers.Count() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count := a.workers.Count(); count != 1 {
		t.Errorf("workers.Count() = %d, want 1 while detail load in flight", count)
	}

	close(callbackDone)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.workers.Count() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count := a.workers.Count(); count != 0 {
		t.Errorf("workers.Count() = %d, want 0 after detail load callback returned", count)
	}
}

// TestDrainWorkers_DrainedOnRunExit confirms that drainWorkers leaves
// the registry in StateClosed.
func TestDrainWorkers_DrainedOnRunExit(t *testing.T) {
	a := NewApp(nil, AppCallbacks{})
	if a.workers.State() != livetrack.StateOpen {
		t.Fatalf("initial registry state = %v, want StateOpen", a.workers.State())
	}

	a.drainWorkers()

	if state := a.workers.State(); state != livetrack.StateClosed {
		t.Errorf("after drainWorkers, state = %v, want StateClosed", state)
	}
}

// TestDrainWorkers_ForceClosesStuckSession confirms that when a worker
// does not release before the drain deadline, drainWorkers force-closes
// it and the registry reaches StateClosed.
func TestDrainWorkers_ForceClosesStuckSession(t *testing.T) {
	a := NewApp(nil, AppCallbacks{})

	closed := make(chan struct{})
	closerFired := false
	var closerMu sync.Mutex
	stubCloser := &testFuncCloser{fn: func() {
		closerMu.Lock()
		closerFired = true
		closerMu.Unlock()
		close(closed)
	}}

	sess, err := a.workers.Register(context.Background(), "async_load", WorkerMeta{
		Kind:     "async_load",
		Source:   "stuck_worker",
		Deadline: time.Time{},
	}, stubCloser)
	if err != nil {
		t.Fatalf("Register returned unexpected err: %v", err)
	}
	_ = sess

	a.drainWorkers()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("testFuncCloser was not called during drain force-close")
	}

	closerMu.Lock()
	fired := closerFired
	closerMu.Unlock()
	if !fired {
		t.Error("closer was not called during force-close")
	}

	if state := a.workers.State(); state != livetrack.StateClosed {
		t.Errorf("after drainWorkers with force-close, state = %v, want StateClosed", state)
	}
}

// TestRequestSessionsAsync_DrainFiresLoadCancel confirms that an in-flight
// async_load goroutine has its context canceled when drainWorkers force-closes
// the registered closer. The stub callback blocks until its context is done,
// so the test can confirm the cancel arrived from the drain path rather than
// from parent context cancellation.
func TestRequestSessionsAsync_DrainFiresLoadCancel(t *testing.T) {
	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel()

	callbackEntered := make(chan struct{})
	cancelObserved := make(chan struct{})

	cb := AppCallbacks{
		ListSessions: func(cbCtx context.Context) (SessionSnapshot, error) {
			close(callbackEntered)
			<-cbCtx.Done()
			close(cancelObserved)
			return SessionSnapshot{}, nil
		},
	}

	a := NewApp(nil, cb, AppOptions{Context: ctx})
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)
	a.screen = scr

	a.requestSessionsAsync("drain-test")

	select {
	case <-callbackEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("ListSessions callback was never invoked")
	}

	// drainWorkers force-closes all registered workers; this must fire the
	// workerCloser cancel wired to the in-flight loadCtx.
	a.drainWorkers()

	select {
	case <-cancelObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("loadCtx was not canceled by drainWorkers force-close")
	}
}

// TestRegistrySubscriberWorkerCloser_FiresStreamCancel confirms that the
// workerCloser registered for an rpc_stream worker fires the stream cancel
// function when Close is called. This simulates the drain force-close path
// for a registry subscriber goroutine that is still reading from a live
// gRPC stream.
func TestRegistrySubscriberWorkerCloser_FiresStreamCancel(t *testing.T) {
	// streamCancelFired tracks whether the stream cancel reached by the
	// workerCloser was actually called.
	streamCancelFired := make(chan struct{})
	streamCancel := func() { close(streamCancelFired) }

	a := NewApp(nil, AppCallbacks{})

	// Simulate what runRegistrySupervisor does: register the subscriber
	// goroutine with a workerCloser wrapping the stream cancel.
	sess, err := a.workers.Register(a.ctx, "rpc_stream", WorkerMeta{
		Kind:     "rpc_stream",
		Source:   "registry_subscriber",
		Deadline: time.Time{},
	}, newWorkerCloser(streamCancel))
	if err != nil {
		t.Fatalf("Register returned unexpected err: %v", err)
	}
	_ = sess

	// drainWorkers force-closes all registered workers; for this
	// rpc_stream session the closer must invoke streamCancel.
	a.drainWorkers()

	select {
	case <-streamCancelFired:
	case <-time.After(2 * time.Second):
		t.Fatal("stream cancel was not fired by drainWorkers force-close")
	}
}

// testFuncCloser is a test-only livetrack.Closer that calls fn on Close.
type testFuncCloser struct {
	fn func()
}

// Close calls fn if set.
func (c *testFuncCloser) Close(string) error {
	if c.fn != nil {
		c.fn()
	}
	return nil
}
