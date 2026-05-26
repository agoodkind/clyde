package livetrack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"goodkind.io/clyde/internal/correlation"
)

// State enumerates the registry lifecycle states. Open is the
// steady-state where Register is permitted. Draining is entered on
// the first Drain call: existing sessions stay tracked but new
// Register calls are rejected with ErrRegistryClosed. Forcing is
// entered when graceful drain hits its deadline with sessions still
// alive. Closed is the terminal state.
type State uint32

// StateOpen, StateDraining, StateForcing, and StateClosed are the
// four states a Registry walks through during its lifetime. New
// Registries start in StateOpen; Drain advances them through
// Draining and (on the deadline path) Forcing before settling on
// StateClosed.
const (
	StateOpen     State = iota // accepts Register calls; Release operates normally.
	StateDraining              // Drain has begun; Register returns ErrRegistryClosed.
	StateForcing               // poll deadline elapsed with sessions remaining; force-close in progress.
	StateClosed                // terminal state; the registry is no longer in use.
)

// String returns the lower-case name of s. It is used in slog
// records so operators see a stable label for each lifecycle state.
func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateDraining:
		return "draining"
	case StateForcing:
		return "forcing"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// ErrRegistryClosed is returned by Register once the registry has
// transitioned out of [StateOpen]. The error is stable so callers
// can match it via [errors.Is] when deciding whether to retry
// against a fresh registry or report a reload-in-progress condition.
var ErrRegistryClosed = errors.New("livetrack: registry closed")

const (
	defaultPollEvery   = 50 * time.Millisecond
	defaultCloserGrace = 2 * time.Second
)

// Options configures a Registry at construction time. All fields are
// optional; zero values fall back to package defaults.
type Options[M Meta] struct {
	// Component identifies the subsystem owning this registry. It is
	// emitted on every slog event so operators can filter by owner.
	Component string
	// Concern is the slog concern key (e.g. "providers.mitm.lifecycle")
	// emitted on every slog event so concern logs route correctly.
	Concern string
	// Log is the base logger. When nil, slog.Default() is used.
	Log *slog.Logger
	// PollEvery is the cadence at which Drain polls Count for the
	// idle exit path. Defaults to 50ms.
	PollEvery time.Duration
	// CloserGrace bounds each Closer.Close call during force-close.
	// Defaults to 2s.
	CloserGrace time.Duration
	// ParallelClose controls whether the closer fan-out runs
	// concurrently across min(NumCPU, n) workers (true) or
	// sequentially in snapshot order (false, default).
	ParallelClose bool
	// Now is the time source for OpenedAt and duration accounting. It
	// defaults to time.Now and exists so tests can drive the clock.
	Now func() time.Time
}

// Session is the public, typed handle returned by Register. Callers
// pass it back to Release. The Meta value is whatever struct the
// adopting subsystem chose to track (e.g. MITMMeta with ConnectHost
// and CaptureFile). The internal pointer fields (corr, closer,
// state) are read-only after Register so callers can copy a Session
// value through Snapshot or ForEach without triggering vet's
// copylocks check.
type Session[M Meta] struct {
	ID       string
	ParentID string
	Kind     string
	OpenedAt time.Time
	Meta     M
	corr     correlation.Context
	closer   Closer
	state    *sessionState
}

// Touch records the current time as the session's most recent
// byte-level activity. The byte-pipe owner calls this after every
// successful read or write on the underlying transport so the drain
// idle-grace fast-path can distinguish wedged sessions from actively
// streaming ones. Safe to call from any goroutine; the underlying
// atomic store is lock-free. Snapshot-copied Session values have a
// nil state pointer and Touch is a no-op on them; only the live
// pointer returned from Register carries a writable state.
//
// The clock reference comes from the registry's injected
// [Options.Now], so tests that drive a fake clock through Register
// also drive Touch's timestamp.
func (s *Session[M]) Touch() {
	if s == nil || s.state == nil || s.state.now == nil {
		return
	}
	s.state.lastActivityNano.Store(s.state.now().UnixNano())
}

// IdleSince returns the wall-clock duration between now and the
// session's most recently recorded activity. The drain idle-grace
// fast-path compares this against its grace window so wedged sessions
// (no byte movement since registration or since the last Touch) get
// force-closed immediately, while sessions whose owners are still
// pushing bytes ride out the deadline path. Returns zero on a
// snapshot-copied Session whose state pointer is nil.
func (s *Session[M]) IdleSince(now time.Time) time.Duration {
	if s == nil || s.state == nil {
		return 0
	}
	last := time.Unix(0, s.state.lastActivityNano.Load())
	return now.Sub(last)
}

// sessionState carries the mutable bits of a tracked session. It
// lives behind a pointer on Session so that Snapshot can return a
// Session value without copying a [sync.Once], [atomic.Bool], or
// [atomic.Int64]. lastActivityNano is the per-session
// last-activity timestamp stored as UnixNano so atomic load and
// store stay lock-free on the hot byte-pipe path; see
// [Session.Touch] and [Session.IdleSince]. now is the registry's
// injected clock so Touch and IdleSince stay testable without
// reaching for the package-level [time.Now].
type sessionState struct {
	once             sync.Once
	released         atomic.Bool
	lastActivityNano atomic.Int64
	now              func() time.Time
}

// sessionEntry is the package-internal record stored in the registry
// map. It strips the Meta type parameter so closer fan-out can speak
// in concrete fields without forcing every helper to be generic.
type sessionEntry struct {
	id       string
	parentID string
	kind     string
	openedAt time.Time
	corr     correlation.Context
	closer   Closer
}

// DrainResult is the outcome of one Drain call. Reason is the
// caller-supplied reason. Final is the terminal state Drain reached.
// Remaining is the count of sessions still tracked when Drain exited
// (zero on the idle path, non-zero on the deadline path before
// force-close). ForceClosed is the count of sessions that went
// through Closer.Close on the deadline path. Errors aggregates
// closer errors and is nil when none fired. Duration measures wall
// time between Drain entry and exit.
type DrainResult struct {
	Reason      string
	Final       State
	Remaining   int
	ForceClosed int
	Errors      []error
	Duration    time.Duration
}

// Predicate selects a subset of sessions for matching operations such
// as ForceCloseMatching and CountByPredicate. The predicate sees a
// snapshot value and must not retain references to internal state.
type Predicate[M Meta] func(Session[M]) bool

// registerCfg captures per-call options applied through RegisterOption
// closures. It exists to keep the Register signature stable while
// allowing parent linkage to be added later without breaking callers.
type registerCfg struct {
	parentID string
}

// RegisterOption configures one Register call.
type RegisterOption func(*registerCfg)

// WithParent links the new session to an existing parent. The parent
// pointer is read once at Register time; passing nil is a no-op so
// callers can compose options without checking.
func WithParent[M Meta](parent *Session[M]) RegisterOption {
	return func(cfg *registerCfg) {
		if parent != nil {
			cfg.parentID = parent.ID
		}
	}
}

// Registry is the per-subsystem session inventory. Construct one with
// New, register sessions with Register, and either Release them when
// they finish or call Drain to fan-out close on whatever remains.
type Registry[M Meta] struct {
	component     string
	concern       string
	log           *slog.Logger
	pollEvery     time.Duration
	closerGrace   time.Duration
	parallelClose bool
	now           func() time.Time

	state atomic.Uint32

	mu       sync.RWMutex
	sessions map[string]*Session[M]
	parents  map[string][]string
}

// New constructs a Registry with the supplied options. Options fields
// are validated and fall back to defaults so callers can pass a
// zero-value Options[M]{} for the common case.
func New[M Meta](opts Options[M]) *Registry[M] {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.Component != "" {
		log = log.With("component", opts.Component)
	}
	if opts.Concern != "" {
		log = log.With("concern", opts.Concern)
	}
	pollEvery := opts.PollEvery
	if pollEvery <= 0 {
		pollEvery = defaultPollEvery
	}
	closerGrace := opts.CloserGrace
	if closerGrace <= 0 {
		closerGrace = defaultCloserGrace
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	r := &Registry[M]{
		component:     opts.Component,
		concern:       opts.Concern,
		log:           log,
		pollEvery:     pollEvery,
		closerGrace:   closerGrace,
		parallelClose: opts.ParallelClose,
		now:           now,
		state:         atomic.Uint32{},
		mu:            sync.RWMutex{},
		sessions:      make(map[string]*Session[M]),
		parents:       make(map[string][]string),
	}
	r.state.Store(uint32(StateOpen))
	return r
}

// State returns the current registry state. Reads are atomic so
// callers can poll without holding the registry lock.
func (r *Registry[M]) State() State {
	return State(r.state.Load())
}

// Register adds a new session to the registry. It is rejected with
// ErrRegistryClosed once Drain has put the registry past StateOpen.
// The returned *Session carries the registered metadata; callers pass
// it to Release when the underlying owner finishes naturally.
func (r *Registry[M]) Register(ctx context.Context, kind string, meta M, closer Closer, opts ...RegisterOption) (*Session[M], error) {
	if r.State() != StateOpen {
		return nil, ErrRegistryClosed
	}
	cfg := registerCfg{parentID: ""}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	id := randomSessionID(r.now)
	openedAt := r.now()
	sess := &Session[M]{
		ID:       id,
		ParentID: cfg.parentID,
		Kind:     kind,
		OpenedAt: openedAt,
		Meta:     meta,
		corr:     correlation.FromContext(ctx),
		closer:   closer,
		state:    &sessionState{once: sync.Once{}, released: atomic.Bool{}, lastActivityNano: atomic.Int64{}, now: r.now},
	}
	sess.state.lastActivityNano.Store(openedAt.UnixNano())
	r.mu.Lock()
	if State(r.state.Load()) != StateOpen {
		r.mu.Unlock()
		return nil, ErrRegistryClosed
	}
	r.sessions[id] = sess
	if cfg.parentID != "" {
		r.parents[cfg.parentID] = append(r.parents[cfg.parentID], id)
	}
	r.mu.Unlock()
	r.emitSessionEvent(ctx, slog.LevelInfo, "livetrack.session.registered", sess, "")
	return sess, nil
}

// Release marks the session finished and removes it from the
// registry. It is idempotent: a second Release is a no-op so callers
// can safely pair it with a defer even when Drain has already
// force-closed the underlying owner. ctx is used for correlation
// attribute attachment on the released slog event; passing
// [context.Background] is acceptable when no caller context is
// available (e.g. in defers after the request context has been
// canceled).
func (r *Registry[M]) Release(ctx context.Context, s *Session[M], reason string) {
	if s == nil || s.state == nil {
		return
	}
	if !s.state.released.CompareAndSwap(false, true) {
		return
	}
	r.mu.Lock()
	delete(r.sessions, s.ID)
	if s.ParentID != "" {
		siblings := r.parents[s.ParentID]
		filtered := siblings[:0]
		for _, child := range siblings {
			if child != s.ID {
				filtered = append(filtered, child)
			}
		}
		if len(filtered) == 0 {
			delete(r.parents, s.ParentID)
		} else {
			r.parents[s.ParentID] = filtered
		}
	}
	r.mu.Unlock()
	r.emitSessionEvent(ctx, slog.LevelInfo, "livetrack.session.released", s, reason)
}

// Snapshot returns a copy of every currently tracked session. Callers
// can iterate the result without holding the registry lock and
// without observing concurrent Register or Release activity.
func (r *Registry[M]) Snapshot() []Session[M] {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Session[M], 0, len(r.sessions))
	for _, sess := range r.sessions {
		out = append(out, snapshotSession(sess))
	}
	return out
}

// Count returns the number of sessions currently tracked.
func (r *Registry[M]) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// CountByPredicate returns the number of sessions for which p
// returns true. The predicate is invoked under the registry lock so
// it must not block.
func (r *Registry[M]) CountByPredicate(p Predicate[M]) int {
	if p == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, sess := range r.sessions {
		if p(snapshotSession(sess)) {
			count++
		}
	}
	return count
}

// ForEach calls f for every tracked session. The callback receives a
// snapshot value so it cannot mutate registry state through the
// session pointer.
func (r *Registry[M]) ForEach(f func(Session[M])) {
	if f == nil {
		return
	}
	for _, sess := range r.Snapshot() {
		f(sess)
	}
}

// ForceCloseMatching runs Closer.Close on every session for which p
// returns true and removes them from the registry. Returns the
// number of sessions that went through close. Callers (e.g. parent
// cascade adopters) drive the predicate; the registry does not
// auto-cascade parent close to children. ctx is used for slog
// routing on the force_close events; correlation back-fills from
// each session's captured context.
func (r *Registry[M]) ForceCloseMatching(ctx context.Context, p Predicate[M], reason string) int {
	if p == nil {
		return 0
	}
	r.mu.Lock()
	matched := make([]sessionEntry, 0)
	matchedSessions := make([]*Session[M], 0)
	for _, sess := range r.sessions {
		if p(snapshotSession(sess)) {
			matched = append(matched, toSessionEntry(sess))
			matchedSessions = append(matchedSessions, sess)
		}
	}
	for _, sess := range matchedSessions {
		delete(r.sessions, sess.ID)
		if sess.ParentID != "" {
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
	}
	r.mu.Unlock()
	for i, entry := range matched {
		r.emitForceCloseEvent(ctx, matchedSessions[i], reason)
		_ = runCloserBounded(r.closerGrace, entry, reason)
		if matchedSessions[i].state != nil {
			matchedSessions[i].state.released.Store(true)
		}
	}
	return len(matched)
}

// snapshotSession copies the public fields of a session for callers.
// It deliberately drops the closer and state pointers so the
// snapshot value cannot be used to drive close or release from
// outside the registry.
func snapshotSession[M Meta](s *Session[M]) Session[M] {
	return Session[M]{
		ID:       s.ID,
		ParentID: s.ParentID,
		Kind:     s.Kind,
		OpenedAt: s.OpenedAt,
		Meta:     s.Meta,
		corr:     s.corr,
		closer:   nil,
		state:    nil,
	}
}

// toSessionEntry projects a *Session into the package-internal record
// the closer fan-out works against.
func toSessionEntry[M Meta](s *Session[M]) sessionEntry {
	return sessionEntry{
		id:       s.ID,
		parentID: s.ParentID,
		kind:     s.Kind,
		openedAt: s.OpenedAt,
		corr:     s.corr,
		closer:   s.closer,
	}
}

// randomSessionID generates a 16-byte hex id from the supplied
// clock fallback when crypto/rand fails. It uses crypto/rand so ids
// are unguessable across processes; the now closure is only
// consulted on the [rand.Read] error path, which on supported
// platforms is essentially impossible.
func randomSessionID(now func() time.Time) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "fallback-" + now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(buf)
}
