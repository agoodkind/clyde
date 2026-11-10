package livetrack

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/gklog/correlation"
)

// testMeta is the marker-bearing metadata struct used throughout the
// livetrack tests. It carries the minimum fields the assertions need
// without pulling in a real subsystem dependency.
type testMeta struct {
	Tag string
}

func (testMeta) IsLivetrackMeta() {}

// noopCloser implements Closer with a counter so tests can assert how
// many times Close fired and force-close fan-out paths can verify
// per-session invocation.
type noopCloser struct {
	count atomic.Int32
	err   error
	delay time.Duration
}

func (c *noopCloser) Close(reason string) error {
	_ = reason
	c.count.Add(1)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return c.err
}

// blockingCloser blocks Close until ctx is canceled. Used by the
// drain-deadline test to simulate Cloudflare keepalive tunnels that
// never finish on their own.
type blockingCloser struct {
	count   atomic.Int32
	release chan struct{}
}

func newBlockingCloser() *blockingCloser {
	return &blockingCloser{count: atomic.Int32{}, release: make(chan struct{})}
}

func (c *blockingCloser) Close(reason string) error {
	_ = reason
	c.count.Add(1)
	<-c.release
	return nil
}

func TestRegisterReleaseRace(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component: "test",
		Concern:   "test.race",
	})
	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			closer := &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
			sess, err := r.Register(context.Background(), "race", testMeta{Tag: "race"}, closer)
			if err != nil {
				t.Errorf("register: %v", err)
				return
			}
			r.Release(context.Background(), sess, "test.race")
		}()
	}
	wg.Wait()
	if got := r.Count(); got != 0 {
		t.Fatalf("count after race: got %d, want 0", got)
	}
}

func TestDrainIdle(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.idle",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	closers := make([]*noopCloser, 8)
	sessions := make([]*Session[testMeta], 8)
	for i := range closers {
		closers[i] = &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
		sess, err := r.Register(context.Background(), "idle", testMeta{Tag: fmt.Sprintf("idle-%d", i)}, closers[i])
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		sessions[i] = sess
	}
	// Release everything asynchronously so Drain hits the idle path.
	go func() {
		for _, sess := range sessions {
			r.Release(context.Background(), sess, "test.idle")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := r.drainWith(ctx, "test.idle", DrainOptions{IdleGrace: 0})
	if result.Final != StateClosed {
		t.Fatalf("final state: got %s, want closed", result.Final)
	}
	if result.Remaining != 0 {
		t.Fatalf("remaining: got %d, want 0", result.Remaining)
	}
	if result.ForceClosed != 0 {
		t.Fatalf("force-closed: got %d, want 0", result.ForceClosed)
	}
	for i, closer := range closers {
		if got := closer.count.Load(); got != 0 {
			t.Fatalf("closer %d ran on idle drain: got %d, want 0", i, got)
		}
	}
}

func TestDrainDeadline(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.deadline",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 1 * time.Second,
	})
	const sessionCount = 4
	closers := make([]*blockingCloser, sessionCount)
	for i := range closers {
		closers[i] = newBlockingCloser()
		_, err := r.Register(context.Background(), "deadline", testMeta{Tag: fmt.Sprintf("deadline-%d", i)}, closers[i])
		if err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	// Release blockers shortly after Drain begins so closers complete.
	go func() {
		time.Sleep(50 * time.Millisecond)
		for _, closer := range closers {
			close(closer.release)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := r.drainWith(ctx, "test.deadline", DrainOptions{IdleGrace: 0})
	if result.Final != StateClosed {
		t.Fatalf("final state: got %s, want closed", result.Final)
	}
	if result.ForceClosed != sessionCount {
		t.Fatalf("force-closed: got %d, want %d", result.ForceClosed, sessionCount)
	}
	for i, closer := range closers {
		if got := closer.count.Load(); got != 1 {
			t.Fatalf("closer %d: got %d invocations, want 1", i, got)
		}
	}
}

func TestRegisterDuringDrainRejected(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.drain.reject",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	blocker := newBlockingCloser()
	_, err := r.Register(context.Background(), "blocker", testMeta{Tag: "blocker"}, blocker)
	if err != nil {
		t.Fatalf("register blocker: %v", err)
	}
	drainStarted := make(chan struct{})
	drainDone := make(chan DrainResult, 1)
	go func() {
		close(drainStarted)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		drainDone <- r.drainWith(ctx, "test.reject", DrainOptions{IdleGrace: 0})
	}()
	<-drainStarted
	// Wait for state to flip to Draining so Register sees the rejected
	// state. PollEvery is small, so a short sleep is enough to let
	// Drain run its CAS.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.State() != StateOpen {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if _, err := r.Register(context.Background(), "late", testMeta{Tag: "late"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}); !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("register during drain: got %v, want ErrRegistryClosed", err)
	}
	close(blocker.release)
	<-drainDone
}

func TestForceCloseIdempotent(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.force.idem",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	closer := &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
	_, err := r.Register(context.Background(), "idem", testMeta{Tag: "idem"}, closer)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pred := func(s Session[testMeta]) bool { return s.Kind == "idem" }
	first := r.ForceCloseMatching(context.Background(), pred, "test.first")
	second := r.ForceCloseMatching(context.Background(), pred, "test.second")
	if first != 1 {
		t.Fatalf("first force-close: got %d, want 1", first)
	}
	if second != 0 {
		t.Fatalf("second force-close: got %d, want 0", second)
	}
	if got := closer.count.Load(); got != 1 {
		t.Fatalf("closer ran %d times, want 1", got)
	}
}

func TestParentTraversal(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component: "test",
		Concern:   "test.parent",
	})
	parentCloser := &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
	parent, err := r.Register(context.Background(), "parent", testMeta{Tag: "parent"}, parentCloser)
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	childCount := 3
	childClosers := make([]*noopCloser, childCount)
	for i := 0; i < childCount; i++ {
		childClosers[i] = &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
		_, err := r.Register(context.Background(), "child", testMeta{Tag: fmt.Sprintf("child-%d", i)}, childClosers[i], WithParent[testMeta](parent))
		if err != nil {
			t.Fatalf("register child %d: %v", i, err)
		}
	}
	if got := r.CountByPredicate(func(s Session[testMeta]) bool { return s.ParentID == parent.ID }); got != childCount {
		t.Fatalf("children of parent: got %d, want %d", got, childCount)
	}
	children := r.ChildrenOf(parent.ID)
	if len(children) != childCount {
		t.Fatalf("ChildrenOf: got %d, want %d", len(children), childCount)
	}
	closed := r.ForceCloseMatching(context.Background(), func(s Session[testMeta]) bool { return s.ID == parent.ID }, "test.parent")
	if closed != 1 {
		t.Fatalf("parent close: got %d, want 1", closed)
	}
	for i, closer := range childClosers {
		if got := closer.count.Load(); got != 0 {
			t.Fatalf("child %d closer ran without explicit cascade: got %d, want 0", i, got)
		}
	}
	if r.Count() != childCount {
		t.Fatalf("children remain: got %d, want %d", r.Count(), childCount)
	}
}

func TestCorrelationPropagation(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.correlation",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	corr := correlation.New("req-correlation-test")
	corr.TraceID = correlation.NewTraceID()
	ctx := correlation.WithContext(context.Background(), corr)
	closer := &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
	sess, err := r.Register(ctx, "corr", testMeta{Tag: "corr"}, closer)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if sess.corr.TraceID != corr.TraceID {
		t.Fatalf("trace id captured: got %q, want %q", sess.corr.TraceID, corr.TraceID)
	}
	r.ForceCloseMatching(ctx, func(s Session[testMeta]) bool { return s.ID == sess.ID }, "test.corr")
	if got := closer.count.Load(); got != 1 {
		t.Fatalf("closer count: got %d, want 1", got)
	}
}

// TestSessionTouchUpdatesLastActivity registers one session, lets a
// short wall-clock interval pass, calls Touch, and asserts the
// post-Touch IdleSince reading drops back near zero. The interval
// has to clear the OS scheduler jitter floor so the assertion is
// asymmetric: pre-Touch idleness exceeds the wait, post-Touch
// idleness sits below a generous ceiling that survives slow CI.
func TestSessionTouchUpdatesLastActivity(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component: "test",
		Concern:   "test.touch.update",
	})
	sess, err := r.Register(context.Background(), "touch", testMeta{Tag: "touch"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	wait := 10 * time.Millisecond
	time.Sleep(wait)
	beforeTouch := sess.IdleSince(time.Now())
	if beforeTouch < wait {
		t.Fatalf("pre-touch idle: got %v, want >= %v", beforeTouch, wait)
	}
	sess.Touch()
	afterTouch := sess.IdleSince(time.Now())
	if afterTouch > 5*time.Millisecond {
		t.Fatalf("post-touch idle: got %v, want < 5ms", afterTouch)
	}
}

// TestSessionTouchRaceFree spawns many goroutines that each call
// Touch many times in parallel and asserts the registry sees no
// data race under `go test -race`. The atomic.Int64 store is the
// load-bearing piece; a regression that swapped it for a mutex
// would surface here as a race detector fault when the test runs
// under -race.
func TestSessionTouchRaceFree(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component: "test",
		Concern:   "test.touch.race",
	})
	sess, err := r.Register(context.Background(), "race", testMeta{Tag: "race"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	const goroutines = 100
	const perGoroutine = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				sess.Touch()
			}
		}()
	}
	wg.Wait()
	// Sanity: a recent Touch leaves IdleSince small.
	if got := sess.IdleSince(time.Now()); got > 100*time.Millisecond {
		t.Fatalf("post-race idle: got %v, want < 100ms", got)
	}
}

// TestSessionIdleSinceMonotonic registers one session, takes two
// IdleSince readings with no Touch in between, and asserts the
// second reading is at least as large as the first. A regression
// that stored the timestamp in a non-monotonic field (e.g. raw
// wall-clock comparison across a stretch where the clock jumps
// backwards) would surface as a smaller second reading.
func TestSessionIdleSinceMonotonic(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component: "test",
		Concern:   "test.idle.monotonic",
	})
	sess, err := r.Register(context.Background(), "mono", testMeta{Tag: "mono"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	first := sess.IdleSince(time.Now())
	time.Sleep(1 * time.Millisecond)
	second := sess.IdleSince(time.Now())
	if second < first {
		t.Fatalf("monotonic: got first=%v second=%v, want second >= first", first, second)
	}
}

func TestCloserErrorAggregation(t *testing.T) {
	t.Parallel()
	r := newRegistry[testMeta](Options[testMeta]{
		Component:   "test",
		Concern:     "test.errors",
		PollEvery:   5 * time.Millisecond,
		CloserGrace: 200 * time.Millisecond,
	})
	const total = 6
	for i := 0; i < total; i++ {
		var closer Closer
		if i%2 == 0 {
			closer = &noopCloser{count: atomic.Int32{}, err: fmt.Errorf("closer %d failed", i), delay: 0}
		} else {
			closer = &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
		}
		_, err := r.Register(context.Background(), "err", testMeta{Tag: fmt.Sprintf("err-%d", i)}, closer)
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	result := r.drainWith(ctx, "test.errors", DrainOptions{IdleGrace: 0})
	if result.Final != StateClosed {
		t.Fatalf("final state: got %s, want closed", result.Final)
	}
	if got := len(result.Errors); got != total/2 {
		t.Fatalf("error count: got %d, want %d", got, total/2)
	}
	joined := errors.Join(result.Errors...)
	if joined == nil {
		t.Fatalf("expected joined errors, got nil")
	}
}
