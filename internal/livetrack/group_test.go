package livetrack

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// groupTestLogger returns a discard logger for group tests so the suite stays
// quiet without dropping the structured-logging call paths.
func groupTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClock is a manually advanced time source injected through Options.Now so
// idle-activity tests drive IdleSince deterministically rather than sleeping.
type fakeClock struct {
	mu  sync.Mutex
	cur time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{mu: sync.Mutex{}, cur: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur = c.cur.Add(d)
}

// TestPhaseOrder pins the declared lifecycle order: ingress drains before
// egress, egress before workers, workers before storage.
func TestPhaseOrder(t *testing.T) {
	t.Parallel()
	want := []Phase{PhaseIngress, PhaseEgress, PhaseWorkers, PhaseStorage}
	got := orderedPhases()
	if len(got) != len(want) {
		t.Fatalf("orderedPhases len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("phase[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestActiveCountUsesLiveActivity asserts ActiveCount(grace) walks live session
// pointers so IdleSince reflects real activity, unlike Snapshot which drops the
// state pointer and would read zero.
func TestActiveCountUsesLiveActivity(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	r := New[testMeta](Options[testMeta]{Component: "test", Concern: "test.active", Now: clock.now})
	s, err := r.Register(context.Background(), "k", testMeta{Tag: "k"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	clock.advance(10 * time.Second)
	if got := r.ActiveCount(5 * time.Second); got != 0 {
		t.Fatalf("idle session counted active: got %d, want 0", got)
	}
	s.Touch()
	if got := r.ActiveCount(5 * time.Second); got != 1 {
		t.Fatalf("touched session not counted active: got %d, want 1", got)
	}
}

// TestQuiesceRunsPhasesInOrder asserts Quiesce runs hooks in phase order, and
// that a hook at PhaseStorage runs after every ingress hook has run.
func TestQuiesceRunsPhasesInOrder(t *testing.T) {
	t.Parallel()
	g := NewGroup(GroupOptions{Log: groupTestLogger()})
	var mu sync.Mutex
	order := []string{}
	record := func(name string) func(context.Context) error {
		return func(context.Context) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	g.AddHook(PhaseIngress, "ingress.hook", record("ingress"))
	g.AddHook(PhaseStorage, "storage.hook", record("storage"))
	g.Quiesce(context.Background(), "test", Budget{Cap: time.Second, IdleGrace: 0})
	if len(order) != 2 || order[0] != "ingress" || order[1] != "storage" {
		t.Fatalf("hook order = %v, want [ingress storage]", order)
	}
}

// TestAwaitQuietReturnsWhenActive asserts AwaitQuiet returns once no
// QuietRelevant member has a session active within grace, and that a
// non-quiet-relevant member (a worker) does not hold it open.
func TestAwaitQuietReturnsWhenActive(t *testing.T) {
	t.Parallel()
	g := NewGroup(GroupOptions{Log: groupTestLogger()})
	ingress := Attach[testMeta](g, MemberSpec{Phase: PhaseIngress, QuietRelevant: true, CancelNoWait: false},
		Options[testMeta]{Component: "ingress", Concern: "test.ingress", PollEvery: time.Millisecond})
	worker := Attach[testMeta](g, MemberSpec{Phase: PhaseWorkers, QuietRelevant: false, CancelNoWait: false},
		Options[testMeta]{Component: "worker", Concern: "test.worker", PollEvery: time.Millisecond})
	if _, err := worker.Register(context.Background(), "job", testMeta{Tag: "job"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	s, err := ingress.Register(context.Background(), "req", testMeta{Tag: "req"}, &noopCloser{count: atomic.Int32{}, err: nil, delay: 0})
	if err != nil {
		t.Fatalf("register ingress: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		ingress.Release(context.Background(), s, "done")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if quiet := g.AwaitQuiet(ctx, 5*time.Second); !quiet {
		t.Fatal("AwaitQuiet returned not-quiet despite released ingress session")
	}
}

// TestAwaitQuietImmediateWhenEmpty asserts AwaitQuiet returns true without
// blocking when no quiet-relevant member has any session.
func TestAwaitQuietImmediateWhenEmpty(t *testing.T) {
	t.Parallel()
	g := NewGroup(GroupOptions{Log: groupTestLogger()})
	_ = Attach[testMeta](g, MemberSpec{Phase: PhaseIngress, QuietRelevant: true, CancelNoWait: false},
		Options[testMeta]{Component: "ingress", Concern: "test.ingress", PollEvery: time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if quiet := g.AwaitQuiet(ctx, 5*time.Second); !quiet {
		t.Fatal("AwaitQuiet should be immediately quiet with no sessions")
	}
}

// TestMemberCountTracksAttach asserts Attach increments the group's member
// count so subsystem migration tests can assert they joined.
func TestMemberCountTracksAttach(t *testing.T) {
	t.Parallel()
	g := NewGroup(GroupOptions{Log: groupTestLogger()})
	if g.MemberCount() != 0 {
		t.Fatalf("new group member count = %d, want 0", g.MemberCount())
	}
	_ = Attach[testMeta](g, MemberSpec{Phase: PhaseIngress, QuietRelevant: true, CancelNoWait: false},
		Options[testMeta]{Component: "a", Concern: "test.a"})
	_ = Attach[testMeta](g, MemberSpec{Phase: PhaseStorage, QuietRelevant: false, CancelNoWait: false},
		Options[testMeta]{Component: "b", Concern: "test.b"})
	if g.MemberCount() != 2 {
		t.Fatalf("member count after 2 Attach = %d, want 2", g.MemberCount())
	}
}
