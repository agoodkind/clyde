package livetrack

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Phase names a lifecycle stage. Quiesce runs phases in orderedPhases order;
// members within a phase drain together. The order encodes the dependency
// chain: stop taking client traffic (ingress), finish provider calls (egress),
// stop internal workers, then close storage so SQLite WALs flush last.
type Phase uint8

// The four lifecycle phases, in drain order.
const (
	PhaseIngress Phase = iota
	PhaseEgress
	PhaseWorkers
	PhaseStorage
)

// orderedPhases returns the phases in the order Quiesce drains them.
func orderedPhases() []Phase {
	return []Phase{PhaseIngress, PhaseEgress, PhaseWorkers, PhaseStorage}
}

// String returns a stable label for slog records.
func (p Phase) String() string {
	switch p {
	case PhaseIngress:
		return "ingress"
	case PhaseEgress:
		return "egress"
	case PhaseWorkers:
		return "workers"
	case PhaseStorage:
		return "storage"
	default:
		return "unknown"
	}
}

// Budget bounds one Quiesce call. Cap is the overall deadline; IdleGrace is
// passed to each member's drain so wedged keepalive sessions are force-closed
// at drain start rather than waited out against Cap.
type Budget struct {
	Cap       time.Duration
	IdleGrace time.Duration
}

// MemberSpec declares how a registry participates in the group at Attach time.
// Phase places the member in the drain order. QuietRelevant marks a member
// whose active sessions count toward AwaitQuiet (the client-exchange surfaces).
// CancelNoWait marks a member whose closer is invoked but whose natural release
// is not waited for on the reload path (the config watcher, which can be the
// caller blocked in its own reload RPC).
type MemberSpec struct {
	Phase         Phase
	QuietRelevant bool
	CancelNoWait  bool
}

// GroupOptions configures a Group.
type GroupOptions struct {
	Log *slog.Logger
}

// hookTiming selects whether a hook runs before or after its phase's member
// drains. Before-hooks reproduce steps the pre-refactor sequence ran ahead of a
// registry drain (adapter keepalives-off before ingress drain, MITM
// server.Shutdown before tunnel drain, adapter http.Server.Shutdown after
// ingress drains but before egress drains). After-hooks run once the phase's
// members have drained (store closes after every surface is drained).
type hookTiming uint8

const (
	hookBefore hookTiming = iota
	hookAfter
)

// hook is a named non-registry lifecycle step (keepalives-off,
// http.Server.Shutdown, store close) run at a declared phase and timing.
type hook struct {
	name   string
	timing hookTiming
	fn     func(context.Context) error
}

// quietPollEvery is the AwaitQuiet poll cadence, matching the registry default
// poll so reload timing stays symmetric across surfaces.
const quietPollEvery = 50 * time.Millisecond

// Group is the single declarative lifecycle plan for a daemon generation. It
// owns every long-lived registry (added via Attach) and every non-registry step
// (added via AddHook), and exposes the only public lifecycle entry points:
// Quiesce (ordered drain) and AwaitQuiet (the one wait loop).
type Group struct {
	log     *slog.Logger
	mu      sync.Mutex
	members []drainMember
	hooks   map[Phase][]hook
}

// NewGroup constructs an empty group.
func NewGroup(opts GroupOptions) *Group {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Group{log: log, mu: sync.Mutex{}, members: nil, hooks: map[Phase][]hook{}}
}

// MemberCount reports how many registries are attached. Used by tests to assert
// a subsystem joined the group.
func (g *Group) MemberCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.members)
}

// Attach constructs a Registry[M] bound to this group. It is the only way to
// obtain a registry once New is unexported, so a registry that is not a group
// member cannot be constructed. Package-level (not a method) because Go methods
// cannot take type parameters.
func Attach[M Meta](g *Group, member MemberSpec, opts Options[M]) *Registry[M] {
	reg := newRegistry[M](opts)
	g.mu.Lock()
	g.members = append(g.members, registryMember[M]{reg: reg, memSpec: member})
	g.mu.Unlock()
	return reg
}

// AddHook registers a named non-registry step to run after the phase's members
// drain during Quiesce. Use AddHookBefore for steps that must precede the
// member drains.
func (g *Group) AddHook(phase Phase, name string, fn func(context.Context) error) {
	g.addHook(phase, hookAfter, name, fn)
}

// AddHookBefore registers a named non-registry step to run before the phase's
// members drain during Quiesce.
func (g *Group) AddHookBefore(phase Phase, name string, fn func(context.Context) error) {
	g.addHook(phase, hookBefore, name, fn)
}

func (g *Group) addHook(phase Phase, timing hookTiming, name string, fn func(context.Context) error) {
	g.mu.Lock()
	g.hooks[phase] = append(g.hooks[phase], hook{name: name, timing: timing, fn: fn})
	g.mu.Unlock()
}

// Quiesce runs the declared lifecycle to completion under budget. For each
// phase in order it drains every member in that phase (concurrently) and runs
// every hook for that phase, bounded by budget.Cap overall and budget.IdleGrace
// per member drain.
func (g *Group) Quiesce(parent context.Context, reason string, budget Budget) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), budget.Cap)
	defer cancel()
	for _, phase := range orderedPhases() {
		g.quiescePhase(ctx, phase, reason, budget)
	}
}

// quiescePhase runs the phase's before-hooks, drains every member in the phase
// concurrently, then runs the phase's after-hooks. The member drain mirrors the
// WaitGroup fan-out in the pre-refactor startReloadDrain so concurrent draining
// is preserved; before/after timing reproduces the per-subsystem ordering the
// hand-sequenced drain ran (keepalives-off and http shutdown around the
// registry drain, store closes after every surface drained).
func (g *Group) quiescePhase(ctx context.Context, phase Phase, reason string, budget Budget) {
	g.mu.Lock()
	members := make([]drainMember, 0)
	for _, m := range g.members {
		if m.spec().Phase == phase {
			members = append(members, m)
		}
	}
	hooks := append([]hook(nil), g.hooks[phase]...)
	g.mu.Unlock()

	for _, h := range hooks {
		if h.timing == hookBefore {
			g.runHook(ctx, phase, h)
		}
	}
	var wg sync.WaitGroup
	for _, m := range members {
		wg.Add(1)
		go func(member drainMember) {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					g.log.ErrorContext(ctx, "livetrack.group.member_panic", "concern", "livetrack",
						"name", member.component(), "err", recovered)
				}
			}()
			member.drainWith(ctx, reason, DrainOptions{IdleGrace: budget.IdleGrace})
		}(m)
	}
	wg.Wait()
	for _, h := range hooks {
		if h.timing == hookAfter {
			g.runHook(ctx, phase, h)
		}
	}
}

// runHook executes one phase hook with panic recovery and structured logging on
// error. A hook failure is logged but does not abort the phase, matching the
// best-effort store-close behavior of the pre-refactor drain.
func (g *Group) runHook(ctx context.Context, phase Phase, h hook) {
	defer func() {
		if recovered := recover(); recovered != nil {
			g.log.ErrorContext(ctx, "livetrack.group.hook_panic", "concern", "livetrack",
				"name", h.name, "err", recovered)
		}
	}()
	if err := h.fn(ctx); err != nil {
		g.log.WarnContext(ctx, "livetrack.group.hook_failed", "concern", "livetrack",
			"phase", phase.String(), "hook", h.name, "err", err)
	}
}

// AwaitQuiet blocks until no QuietRelevant member has a session active within
// grace, or ctx expires. It mutates nothing: registries stay open and new
// sessions register freely while waiting, so this is safe to call before a
// reload that may not happen. Returns true if quiet was reached, false if ctx
// fired first. The single ticker evaluates the quiet predicate across all
// quiet-relevant members on one tick, so quiet is one coherent condition rather
// than N sequential waits.
func (g *Group) AwaitQuiet(ctx context.Context, grace time.Duration) bool {
	if g.quietActiveCount(grace) == 0 {
		return true
	}
	ticker := time.NewTicker(quietPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return g.quietActiveCount(grace) == 0
		case <-ticker.C:
			if g.quietActiveCount(grace) == 0 {
				return true
			}
		}
	}
}

// quietActiveCount sums the active-within-grace session counts of every
// quiet-relevant member, evaluated on one tick so quiet is one coherent
// condition.
func (g *Group) quietActiveCount(grace time.Duration) int {
	g.mu.Lock()
	members := append([]drainMember(nil), g.members...)
	g.mu.Unlock()
	total := 0
	for _, m := range members {
		if m.spec().QuietRelevant {
			total += m.activeCount(grace)
		}
	}
	return total
}
