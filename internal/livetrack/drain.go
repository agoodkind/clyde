package livetrack

import (
	"context"
	"log/slog"
	"time"
)

// DrainOptions configures one [Registry.DrainWith] call. The
// zero-value preserves the historical [Registry.Drain] behavior:
// transition to Draining, wait for natural release until ctx fires,
// then force-close survivors. Fields that need a non-zero default are
// documented on the field itself.
type DrainOptions struct {
	// IdleGrace controls the pre-poll force-close fast-path. When
	// greater than zero, sessions whose [Session.IdleSince] exceeds
	// IdleGrace at drain start get force-closed immediately so the
	// deadline-wait path only covers sessions that are still moving
	// bytes. Use zero to keep the legacy wait-then-deadline
	// behavior; the empirical case for IdleGrace is the daemon
	// reload drain (CLYDE-437 follow-up): MITM proxy connections
	// pinned open by upstream keepalive (Cursor, Claude Code) never
	// emit FIN on their own, so the deadline cap was the only path
	// closing them, and force-close fired one hour after every
	// reload.
	IdleGrace time.Duration
}

// drainWith transitions the registry from Open to Draining and runs
// the configurable drain pipeline. The pipeline is:
//
//  1. If opts.IdleGrace > 0, snapshot the sessions and force-close
//     any whose [Session.IdleSince] exceeds the grace window. This is
//     the pre-poll fast-path the reload-drain logic uses to evict
//     wedged keepalive tunnels before they pin the cap.
//  2. Poll Count at PollEvery until it reaches zero (idle path) or
//     ctx fires (deadline path).
//  3. On the deadline path, force-close every session still tracked
//     before transitioning to Closed.
//
// Calling DrainWith a second time after the registry has left Open
// returns a DrainResult with Final set to the current state and
// Reason "already_draining" so concurrent reload paths can detect
// the duplicate without racing.
func (r *Registry[M]) drainWith(ctx context.Context, reason string, opts DrainOptions) DrainResult {
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

	idleForceClosed, idleErrs := r.runIdleGraceFastPath(ctx, reason, opts)

	if remaining := r.waitForIdle(ctx); remaining == 0 {
		r.state.Store(uint32(StateClosed))
		duration := r.now().Sub(started)
		result := DrainResult{
			Reason:      reason,
			Final:       StateClosed,
			Remaining:   0,
			ForceClosed: idleForceClosed,
			Errors:      idleErrs,
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
	combinedErrs := idleErrs
	if len(errs) > 0 {
		combinedErrs = append(combinedErrs, errs...)
	}
	result := DrainResult{
		Reason:      reason,
		Final:       StateClosed,
		Remaining:   len(entries),
		ForceClosed: idleForceClosed + len(entries),
		Errors:      combinedErrs,
		Duration:    duration,
	}
	r.emitDrainComplete(ctx, result)
	return result
}

// runIdleGraceFastPath force-closes sessions whose last-activity
// timestamp is older than opts.IdleGrace at drain start. Returns the
// number of sessions evicted and any closer errors. When IdleGrace is
// zero the fast-path is skipped and the function returns 0, nil so
// the legacy wait-then-deadline behavior is preserved bit-for-bit.
//
// The fast-path runs after the registry has transitioned to
// StateDraining so Register calls are already rejected; the snapshot
// is taken under the registry lock and the matched sessions are
// removed from the maps in the same critical section, mirroring the
// snapshotEntriesForForce contract used by the deadline path.
func (r *Registry[M]) runIdleGraceFastPath(ctx context.Context, reason string, opts DrainOptions) (int, []error) {
	if opts.IdleGrace <= 0 {
		return 0, nil
	}
	now := r.now()
	entries, sessions := r.snapshotIdleEntries(now, opts.IdleGrace)
	if len(entries) == 0 {
		return 0, nil
	}
	r.emitIdleForceClosed(ctx, reason, len(entries), opts.IdleGrace)
	for _, sess := range sessions {
		r.emitForceCloseEvent(ctx, sess, "drain.idle_grace_exceeded")
	}
	forceCtx := context.WithoutCancel(ctx)
	errs := closerFanOut(forceCtx, r.parallelClose, r.closerGrace, entries, "drain.idle_grace_exceeded")
	r.markEntriesReleased(sessions)
	return len(entries), errs
}

// snapshotIdleEntries returns the session entries and pointers for
// every session whose IdleSince(now) exceeds grace, removing them
// from the registry maps under the registry lock. The split mirrors
// snapshotEntriesForForce so the same close fan-out helper handles
// both paths.
func (r *Registry[M]) snapshotIdleEntries(now time.Time, grace time.Duration) ([]sessionEntry, []*Session[M]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := make([]sessionEntry, 0)
	sessions := make([]*Session[M], 0)
	for _, sess := range r.sessions {
		if sess.IdleSince(now) <= grace {
			continue
		}
		entries = append(entries, toSessionEntry(sess))
		sessions = append(sessions, sess)
	}
	for _, sess := range sessions {
		delete(r.sessions, sess.ID)
		if sess.ParentID == "" {
			continue
		}
		siblings := r.parents[sess.ParentID]
		filtered := siblings[:0]
		for _, child := range siblings {
			if child != sess.ID {
				filtered = append(filtered, child)
			}
		}
		if len(filtered) == 0 {
			delete(r.parents, sess.ParentID)
		} else {
			r.parents[sess.ParentID] = filtered
		}
	}
	return entries, sessions
}

// emitIdleForceClosed records the info-level idle-grace fast-path
// event. The event name is stable so operators can alert on it and
// distinguish wedged-keepalive evictions from the deadline-path
// force-close. The base logger already carries the component and
// concern attributes set in [New]; kind here is the same subsystem
// label so the message is self-describing when grepped in isolation.
func (r *Registry[M]) emitIdleForceClosed(ctx context.Context, reason string, count int, grace time.Duration) {
	if r.log == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("kind", r.component),
		slog.Int("count", count),
		slog.Int64("idle_grace_ms", grace.Milliseconds()),
		slog.String("reason", reason),
	}
	attrs = appendCorrelationAttrsCtxOnly(attrs, ctx)
	r.log.LogAttrs(ctx, slog.LevelInfo, "livetrack.drain.idle_force_closed", append([]slog.Attr{
		// waitForIdle polls Count at PollEvery until it reaches zero or ctx
		// fires. Returns the final count (zero on the idle path, the
		// remaining count on the deadline path so the caller knows how many
		// sessions to force-close).
		slog.String("concern", "livetrack"),
	}, attrs...)...)
}

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
