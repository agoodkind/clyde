package livetrack

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloserFanOutSequential(t *testing.T) {
	t.Parallel()
	closers := make([]*noopCloser, 4)
	entries := make([]sessionEntry, len(closers))
	for i := range closers {
		closers[i] = &noopCloser{count: atomic.Int32{}, err: nil, delay: 0}
		entries[i] = sessionEntry{
			id:     "seq",
			closer: closers[i],
		}
	}
	errs := closerFanOut(context.Background(), false, 200*time.Millisecond, entries, "test.seq")
	if len(errs) != 0 {
		t.Fatalf("errors: got %v, want none", errs)
	}
	for i, closer := range closers {
		if got := closer.count.Load(); got != 1 {
			t.Fatalf("closer %d: got %d, want 1", i, got)
		}
	}
}

func TestCloserFanOutParallel(t *testing.T) {
	t.Parallel()
	closers := make([]*noopCloser, 8)
	entries := make([]sessionEntry, len(closers))
	for i := range closers {
		closers[i] = &noopCloser{count: atomic.Int32{}, err: nil, delay: 5 * time.Millisecond}
		entries[i] = sessionEntry{
			id:     "par",
			closer: closers[i],
		}
	}
	errs := closerFanOut(context.Background(), true, 200*time.Millisecond, entries, "test.par")
	if len(errs) != 0 {
		t.Fatalf("errors: got %v, want none", errs)
	}
	for i, closer := range closers {
		if got := closer.count.Load(); got != 1 {
			t.Fatalf("closer %d: got %d, want 1", i, got)
		}
	}
}

// panickingCloser triggers a runtime panic during Close so the
// fan-out's recover path is exercised.
type panickingCloser struct {
	count atomic.Int32
}

func (c *panickingCloser) Close(reason string) error {
	_ = reason
	c.count.Add(1)
	panic("livetrack: synthetic closer panic")
}

func TestCloserPanicRecovered(t *testing.T) {
	t.Parallel()
	closer := &panickingCloser{count: atomic.Int32{}}
	entry := sessionEntry{id: "panic", closer: closer}
	errs := closerFanOut(context.Background(), false, 200*time.Millisecond, []sessionEntry{entry}, "test.panic")
	if len(errs) != 1 {
		t.Fatalf("errors: got %d, want 1", len(errs))
	}
	if got := closer.count.Load(); got != 1 {
		t.Fatalf("closer count: got %d, want 1", got)
	}
}

// CloserFunc adapter check. Exists so the deadcode analyzer sees a
// reference path; the production code uses CloserFunc when adopters
// pass a closure instead of a typed Closer.
func TestCloserFuncAdapter(t *testing.T) {
	t.Parallel()
	called := atomic.Bool{}
	cf := CloserFunc(func(reason string) error {
		_ = reason
		called.Store(true)
		return nil
	})
	if err := cf.Close("test"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !called.Load() {
		t.Fatalf("CloserFunc was not invoked")
	}
	var nilCF CloserFunc
	if err := nilCF.Close("nil"); err != nil {
		t.Fatalf("nil close: %v", err)
	}
}
