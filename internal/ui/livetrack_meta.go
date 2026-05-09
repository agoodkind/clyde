package ui

import (
	"context"
	"sync/atomic"
	"time"
)

// WorkerMeta is the per-worker metadata stored alongside each
// livetrack.Session the TUI registers. It carries enough information
// to identify the kind of background work and its expected lifetime
// so operators can correlate force-close events with the goroutine
// that owned them.
//
// IsLivetrackMeta satisfies the livetrack.Meta constraint; its body
// is intentionally empty.
type WorkerMeta struct {
	// Kind is one of "draw", "rpc_stream", "watcher", or "async_load".
	Kind string
	// Source is a human-readable label for the specific goroutine or
	// supervisor (e.g. "sessions_load", "registry_supervisor").
	Source string
	// Deadline is the wall-clock time beyond which the worker should
	// not still be alive. Zero means no expected deadline.
	Deadline time.Time
}

// IsLivetrackMeta satisfies the livetrack.Meta marker constraint.
func (WorkerMeta) IsLivetrackMeta() {}

// workerCloser implements livetrack.Closer for a TUI background
// goroutine. Force-close cancels the stream or derived context so the
// goroutine exits its select or channel range on the next iteration.
// The closer is idempotent: the first Close call fires the cancel;
// subsequent calls are no-ops.
type workerCloser struct {
	closed atomic.Bool
	cancel context.CancelFunc
}

func newWorkerCloser(cancel context.CancelFunc) *workerCloser {
	return &workerCloser{closed: atomic.Bool{}, cancel: cancel}
}

// Close cancels the goroutine's stream or context. It is safe to call
// from any goroutine and is idempotent.
func (c *workerCloser) Close(string) error {
	if c.closed.CompareAndSwap(false, true) {
		if c.cancel != nil {
			c.cancel()
		}
	}
	return nil
}

type supervisorStopper struct {
	closed atomic.Bool
	ch     chan struct{}
}

func newSupervisorStopper() *supervisorStopper {
	return &supervisorStopper{closed: atomic.Bool{}, ch: make(chan struct{})}
}

func (s *supervisorStopper) stop() {
	if s != nil && s.closed.CompareAndSwap(false, true) {
		close(s.ch)
	}
}

func (s *supervisorStopper) done() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.ch
}

// supervisorCloser stops a supervisor goroutine by closing the stop channel
// shared with App.Run. The closer and normal Run cleanup share the same
// idempotent stopper, so force-close and normal shutdown cannot double-close.
type supervisorCloser struct {
	stopper *supervisorStopper
}

func newSupervisorCloser(stopper *supervisorStopper) *supervisorCloser {
	return &supervisorCloser{stopper: stopper}
}

// Close stops the supervisor goroutine.
func (c *supervisorCloser) Close(string) error {
	if c.stopper != nil {
		c.stopper.stop()
	}
	return nil
}
