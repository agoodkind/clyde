package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"time"
)

// reloadDrainCap is the outer wall-clock bound on the old generation's
// in-flight drain across every long-lived listener subsystem (MITM
// proxy, adapter HTTP/SSE, dashboard webapp). The replacement daemon
// already owns the inherited listener file descriptors and is serving
// new connections, so the old generation can keep forwarding bytes for
// any stream that was in flight when reload fired without holding up
// the reload RPC. The cap covers actively-streaming sessions only; the
// reloadDrainIdleGrace fast-path evicts wedged keepalive sessions
// before the cap timer starts, so the cap no longer needs to dwarf the
// longest plausible LLM response. Force-close fires only after this
// cap elapses with active sessions still streaming.
//
// The pre-grace generation set this to one hour so wedged
// Cloudflare-keepalive tunnels (Cursor, Claude Code) could not pin
// the old worker forever. The empirical cost was that every reload
// did pin the old worker for an hour because those wedged tunnels
// never closed. Pairing a 60s cap with a 5s idle grace shifts the
// eviction policy from "wait everyone out" to "evict the silent ones
// now, give the active ones a minute", which is the behavior every
// real LLM stream needs and no idle keepalive deserves
// (CLYDE-270, CLYDE-324, CLYDE-437 follow-up).
const reloadDrainCap = 60 * time.Second

// reloadDrainIdleGrace is the window of byte silence after which a
// drained session is force-closed at drain start. Sessions still
// moving bytes (byte-pipe Touch calls in MITM splice and stream
// paths refresh livetrack last-activity timestamps) are spared the
// pre-poll force-close and waited on until reloadDrainCap fires.
const reloadDrainIdleGrace = 5 * time.Second

// reloadDrainSurface is the per-surface contract the shared graceful
// drain orchestrator consumes. Each long-lived listener subsystem
// (adapter HTTP, MITM proxy, dashboard webapp) provides one when its
// drainReloaded* function delegates to runReloadDrain. The hooks shape
// matches the existing per-process struct fields (adapterProcess,
// mitmProcess, webAppProcess) so adoption is a wiring change rather
// than a behaviour change.
type reloadDrainSurface struct {
	// Kind is the surface tag used to compose log event names
	// ("adapter", "mitm", "webapp"). The orchestrator emits events
	// named "daemon.reload.<kind>_drain_active",
	// "daemon.reload.<kind>_drain_complete", and
	// "daemon.reload.<kind>_drain_timeout" so existing log tooling
	// continues to route by event name.
	Kind string
	// Log is the surface-scoped logger; the orchestrator does not
	// rewrite or re-derive it.
	Log *slog.Logger
	// Addr is the listener address recorded on every emitted event.
	Addr string
	// ActiveField is the slog key used for the "how many sessions are
	// in flight" count on log lines ("active_tunnels" for MITM,
	// "active_requests" for adapter, "active_channels" for webapp).
	// Preserved per-surface so existing dashboards keep working.
	ActiveField string
	// ReloadDraining is the race guard the per-surface synchronous
	// cancel function (e.g. exclusive.stop("reload_handoff")) checks
	// before invoking its own bounded force-close. The orchestrator
	// flips this to true before any drain work begins, so a cancel
	// firing concurrently observes the flag and becomes a no-op
	// while the async drain owns the lifecycle. Optional; pass nil
	// when the surface has no separate cancel path.
	ReloadDraining *atomic.Bool

	// CloseListener closes the inherited listener so no new
	// connections land on the old generation. Called synchronously.
	CloseListener func() error
	// ActiveCount returns the current in-flight session count for
	// this surface. Used both for the idle fast-path and for the
	// active-count fields on emitted log lines.
	ActiveCount func() int
	// Drain runs the surface's bounded wait for natural release of
	// in-flight sessions. The orchestrator gives it a fresh
	// context.Background-derived ctx with reloadDrainCap so the
	// caller's request context cancelling on RPC return does not
	// short-circuit the drain.
	Drain func(context.Context) error
	// ForceClose terminates whatever sessions remain after Drain
	// returns. Force-close is the safety net for the cap path
	// only; on the natural-completion path the registry is already
	// empty and ForceClose is a no-op.
	ForceClose func() error
}

// runReloadDrain executes the shared graceful drain handshake for one
// surface. The synchronous portion closes the listener so no new
// connections land on the old generation and force-closes the surface
// when it is already idle. The asynchronous portion (only on the
// active-sessions path) waits up to reloadDrainCap for natural release
// of in-flight sessions and only force-closes if the cap elapses with
// sessions still alive. Returns a done channel the caller and the
// daemon Run loop wait on so the OS process stays alive until the
// drain signals done.
//
// parentCtx propagates correlation (request id, trace id) into the
// background drain ctx via [context.WithoutCancel]; the parent's
// cancellation is intentionally stripped because the reload RPC's
// request context cancels the moment its handler returns, and the
// fire-and-forget drain must outlast that.
func runReloadDrain(parentCtx context.Context, s reloadDrainSurface) <-chan struct{} {
	done := make(chan struct{})
	if s.Log == nil || s.Drain == nil {
		close(done)
		return done
	}
	if s.ReloadDraining != nil {
		s.ReloadDraining.Store(true)
	}
	s.logDrainingStarted(parentCtx)
	s.closeListenerOrLog(parentCtx)
	initialActive := s.initialActiveCount()
	if initialActive == 0 {
		s.runForceCloseIdle(parentCtx)
		s.logDrainComplete(parentCtx, 0)
		close(done)
		return done
	}
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				s.Log.WarnContext(parentCtx, "daemon.reload.drain_panicked",
					"component", "daemon",
					"kind", s.Kind,
					"addr", s.Addr,
					"panic", r,
				)
			}
		}()
		s.runActiveDrain(parentCtx, initialActive)
	}()
	return done
}

func (s reloadDrainSurface) logDrainingStarted(ctx context.Context) {
	s.Log.InfoContext(ctx, "daemon.reload.draining_old_"+s.Kind,
		"component", "daemon",
		"addr", s.Addr,
		"cap", reloadDrainCap.String(),
	)
}

func (s reloadDrainSurface) closeListenerOrLog(ctx context.Context) {
	if s.CloseListener == nil {
		return
	}
	if err := s.CloseListener(); err != nil && !errors.Is(err, net.ErrClosed) {
		s.Log.WarnContext(ctx, "daemon.reload."+s.Kind+"_listener_close_failed",
			"component", "daemon",
			"addr", s.Addr,
			"err", err,
		)
	}
}

func (s reloadDrainSurface) initialActiveCount() int {
	if s.ActiveCount == nil {
		return 0
	}
	return s.ActiveCount()
}

func (s reloadDrainSurface) runForceCloseIdle(ctx context.Context) {
	if s.ForceClose == nil {
		return
	}
	if err := s.ForceClose(); err != nil {
		s.Log.DebugContext(ctx, "daemon.reload."+s.Kind+"_idle_force_close_returned_err",
			"component", "daemon",
			"addr", s.Addr,
			"err", err,
		)
	}
}

func (s reloadDrainSurface) runActiveDrain(parentCtx context.Context, initialActive int) {
	s.Log.InfoContext(parentCtx, "daemon.reload."+s.Kind+"_drain_active",
		"component", "daemon",
		"addr", s.Addr,
		s.activeField(), initialActive,
		"cap", reloadDrainCap.String(),
	)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), reloadDrainCap)
	defer cancel()
	err := s.Drain(ctx)
	finalActive := 0
	if s.ActiveCount != nil {
		finalActive = s.ActiveCount()
	}
	if err != nil {
		s.Log.WarnContext(parentCtx, "daemon.reload."+s.Kind+"_drain_timeout",
			"component", "daemon",
			"addr", s.Addr,
			s.activeField(), finalActive,
			"cap", reloadDrainCap.String(),
			"err", err,
		)
	} else {
		s.logDrainComplete(parentCtx, finalActive)
	}
	if s.ForceClose != nil {
		if err := s.ForceClose(); err != nil {
			s.Log.DebugContext(parentCtx, "daemon.reload."+s.Kind+"_force_close_returned_err",
				"component", "daemon",
				"addr", s.Addr,
				"err", err,
			)
		}
	}
}

func (s reloadDrainSurface) logDrainComplete(ctx context.Context, finalActive int) {
	s.Log.InfoContext(ctx, "daemon.reload."+s.Kind+"_drain_complete",
		"component", "daemon",
		"addr", s.Addr,
		s.activeField(), finalActive,
	)
}

func (s reloadDrainSurface) activeField() string {
	if s.ActiveField == "" {
		return "active"
	}
	return s.ActiveField
}
