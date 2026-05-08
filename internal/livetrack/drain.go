package livetrack

import (
	"context"
	"time"
)

// Drain transitions the registry from Open to Draining, polls Count
// at PollEvery until it reaches zero (idle path) or ctx fires
// (deadline path), and on the deadline path force-closes every
// remaining session before transitioning to Closed.
//
// Calling Drain a second time after the registry has left Open
// returns a DrainResult with Final set to the current state and
// Reason "already_draining" so concurrent reload paths can detect
// the duplicate without racing.
func (r *Registry[M]) Drain(ctx context.Context, reason string) DrainResult {
	if !r.state.CompareAndSwap(uint32(StateOpen), uint32(StateDraining)) {
		return DrainResult{
			Reason:      "already_draining",
			Final:       r.State(),
			Remaining:   r.Count(),
			ForceClosed: 0,
			Errors:      nil,
			Duration:    0,
		}
	}
	started := r.now()
	r.emitDrainStarted(ctx, reason)

	if remaining := r.waitForIdle(ctx); remaining == 0 {
		r.state.Store(uint32(StateClosed))
		duration := r.now().Sub(started)
		result := DrainResult{
			Reason:      reason,
			Final:       StateClosed,
			Remaining:   0,
			ForceClosed: 0,
			Errors:      nil,
			Duration:    duration,
		}
		r.emitDrainComplete(ctx, result)
		return result
	}

	r.state.Store(uint32(StateForcing))
	entries, sessions := r.snapshotEntriesForForce()
	for _, sess := range sessions {
		r.emitForceCloseEvent(ctx, sess, reason)
	}
	// Force-close fan-out runs against a context derived from the
	// drain ctx but stripped of its cancellation, so an already-
	// expired drain ctx does not short-circuit the closers. Each
	// closer is bounded by CloserGrace independently.
	forceCtx := context.WithoutCancel(ctx)
	errs := closerFanOut(forceCtx, r.parallelClose, r.closerGrace, entries, reason)
	r.markEntriesReleased(sessions)
	r.state.Store(uint32(StateClosed))
	duration := r.now().Sub(started)
	result := DrainResult{
		Reason:      reason,
		Final:       StateClosed,
		Remaining:   len(entries),
		ForceClosed: len(entries),
		Errors:      errs,
		Duration:    duration,
	}
	r.emitDrainComplete(ctx, result)
	return result
}

// waitForIdle polls Count at PollEvery until it reaches zero or ctx
// fires. Returns the final count (zero on the idle path, the
// remaining count on the deadline path so the caller knows how many
// sessions to force-close).
func (r *Registry[M]) waitForIdle(ctx context.Context) int {
	if r.Count() == 0 {
		return 0
	}
	ticker := time.NewTicker(r.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return r.Count()
		case <-ticker.C:
			if r.Count() == 0 {
				return 0
			}
		}
	}
}

// snapshotEntriesForForce takes a snapshot of every remaining session
// and atomically removes them from the registry maps. The caller
// runs Closer.Close on the returned entries outside the registry
// lock so a slow closer cannot block concurrent readers.
func (r *Registry[M]) snapshotEntriesForForce() ([]sessionEntry, []*Session[M]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := make([]sessionEntry, 0, len(r.sessions))
	sessions := make([]*Session[M], 0, len(r.sessions))
	for _, sess := range r.sessions {
		entries = append(entries, toSessionEntry(sess))
		sessions = append(sessions, sess)
	}
	r.sessions = make(map[string]*Session[M])
	r.parents = make(map[string][]string)
	return entries, sessions
}

// markEntriesReleased flips the released flag on each session pointer
// so a late Release call from the owner is a no-op.
func (r *Registry[M]) markEntriesReleased(sessions []*Session[M]) {
	for _, sess := range sessions {
		if sess.state != nil {
			sess.state.released.Store(true)
		}
	}
}
