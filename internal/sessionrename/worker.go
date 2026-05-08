package sessionrename

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"goodkind.io/clyde/internal/session"
)

// Renamer is the daemon-side rename mutation point. The worker calls
// Apply for every accepted candidate; the implementation must funnel
// through Server.RenameSession so the live-lease gate, the
// auto_name_source attribution, and the KIND_SESSION_RENAMED event
// stay authoritative.
type Renamer interface {
	Apply(ctx context.Context, oldName, newName string, source session.AutoNameSource, hash string) error
}

// LeaseChecker reports whether a wrapper currently holds the named
// session. The daemon implements it by reading liveSessions and
// liveLeases under the existing remote-control mutex.
type LeaseChecker interface {
	HasLiveLease(name string) bool
}

// LeaseCheckerFunc is the function-shape adapter for LeaseChecker
// so daemon tests can inject behavior without a struct.
type LeaseCheckerFunc func(name string) bool

// HasLiveLease delegates to the underlying function.
func (f LeaseCheckerFunc) HasLiveLease(name string) bool { return f(name) }

// MetadataStore is the persistence boundary the worker writes
// AutoNameState, AutoNameSource, AutoNameSourceHash, and
// LastAutoNameAt through after a successful rename. The daemon
// implements it on its session.FileStore.
type MetadataStore interface {
	UpdateMetadata(name string, mutate func(*session.Metadata)) error
}

// Clock returns the wall-clock time the worker uses for cooldown
// arithmetic. Tests inject a stable clock; production passes
// time.Now.
type Clock func() time.Time

// Worker drives the auto-rename apply pipeline. It is constructed
// once at daemon start and run by repeatedly calling Evaluate from
// the registry-event subscriber goroutine. The Worker keeps no
// in-memory rate-limit state; every gate decision derives from the
// session metadata the daemon already persists, so a daemon reload
// resumes seamlessly without a warm-up period.
type Worker struct {
	cfg     Config
	rename  Renamer
	leases  LeaseChecker
	stores  MetadataStore
	clock   Clock
	logger  *slog.Logger
	limitMu sync.Mutex
	tokens  []time.Time
}

// Config is the worker-level subset of AutoNameConfig (PR3). The
// worker keeps its own typed shape so test cases can construct it
// without importing the config package.
type Config struct {
	Enabled         bool
	MinUserMessages int
	Cooldown        time.Duration
	MaxCallsPerHour int
}

// NewWorker constructs the apply engine. The daemon supplies the
// renamer, the lease checker, the metadata store, the clock, and
// the logger; tests swap each piece independently.
func NewWorker(cfg Config, rename Renamer, leases LeaseChecker, stores MetadataStore, clock Clock, logger *slog.Logger) *Worker {
	if clock == nil {
		clock = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		cfg:     cfg,
		rename:  rename,
		leases:  leases,
		stores:  stores,
		clock:   clock,
		logger:  logger,
		limitMu: sync.Mutex{},
		tokens:  nil,
	}
}

// Evaluate runs the trigger gates plus the source-hash dedupe and,
// when every gate clears, applies the candidate rename. The return
// values let the daemon attribute each evaluation in slog without
// re-running the gates.
type EvaluateResult struct {
	Applied  bool
	OldName  string
	NewName  string
	Reason   string
	Hash     string
	Skipped  bool
	GateOnly bool
}

// errSourceUnchanged signals the candidate-source hash matched what
// was already on the session metadata. The worker returns this so
// the caller knows nothing changed since the last attempt.
var errSourceUnchanged = errors.New("sessionrename: source unchanged")

// errRateLimited signals the global rate limiter rejected the call.
var errRateLimited = errors.New("sessionrename: rate limited")

// Evaluate looks at sess and decides whether to propose, validate,
// and apply a candidate name. The function never blocks on the LLM
// adapter; PR5 introduces a separate code path for that.
func (w *Worker) Evaluate(ctx context.Context, sess *session.Session, transcriptPath string, taken map[string]bool) (EvaluateResult, error) {
	if !w.cfg.Enabled {
		return EvaluateResult{Skipped: true, Reason: "disabled"}, nil
	}
	if sess == nil {
		return EvaluateResult{Skipped: true, Reason: "nil_session"}, nil
	}

	now := w.clock()
	leaseHeld := false
	if w.leases != nil {
		leaseHeld = w.leases.HasLiveLease(sess.Name)
	}

	src, err := ExtractFromTranscript(transcriptPath)
	if err != nil {
		if errors.Is(err, errNoUsableSource) {
			return EvaluateResult{Skipped: true, Reason: "no_source"}, nil
		}
		return EvaluateResult{}, fmt.Errorf("extract source: %w", err)
	}

	gate := EvaluateGates(sess, src.UserMsgCount, leaseHeld, now, GatesConfig{
		MinUserMessages: w.cfg.MinUserMessages,
		Cooldown:        w.cfg.Cooldown,
	})
	if !gate.Allow {
		return EvaluateResult{Skipped: true, GateOnly: true, Reason: gate.Reason}, nil
	}

	if sess.Metadata.AutoNameSourceHash != "" && sess.Metadata.AutoNameSourceHash == src.Hash {
		return EvaluateResult{Skipped: true, Reason: "source_unchanged", Hash: src.Hash}, errSourceUnchanged
	}

	if !w.consumeRateLimit(now) {
		return EvaluateResult{Skipped: true, Reason: "rate_limited"}, errRateLimited
	}

	candidate, err := Propose(src, taken)
	if err != nil {
		w.releaseRateLimit()
		return EvaluateResult{Skipped: true, Reason: candidate.Reason}, err
	}

	if w.rename == nil {
		return EvaluateResult{}, errors.New("sessionrename: renamer not configured")
	}
	if err := w.rename.Apply(ctx, sess.Name, candidate.Name, session.AutoNameSourceTranscript, src.Hash); err != nil {
		return EvaluateResult{}, fmt.Errorf("apply rename: %w", err)
	}

	if w.stores != nil {
		_ = w.stores.UpdateMetadata(candidate.Name, func(md *session.Metadata) {
			md.AutoNameState = session.AutoNameStateApplied
			md.AutoNameSource = session.AutoNameSourceTranscript
			md.AutoNameSourceHash = src.Hash
			md.LastAutoNameAt = now
		})
	}

	w.logger.LogAttrs(ctx, slog.LevelInfo, "daemon.autoname.applied",
		slog.String("component", "daemon"),
		slog.String("subcomponent", "autoname"),
		slog.String("session", sess.Name),
		slog.String("session_id", sess.Metadata.ProviderSessionID()),
		slog.String("source", string(session.AutoNameSourceTranscript)),
		slog.String("prior_name", sess.Name),
		slog.String("new_name", candidate.Name),
		slog.String("hash", src.Hash),
	)

	return EvaluateResult{
		Applied: true,
		OldName: sess.Name,
		NewName: candidate.Name,
		Hash:    src.Hash,
	}, nil
}

// consumeRateLimit applies a sliding-window rate limit using the
// configured MaxCallsPerHour. Zero or negative MaxCallsPerHour
// disables rate limiting entirely. The window is one hour wide,
// anchored at the current time.
func (w *Worker) consumeRateLimit(now time.Time) bool {
	if w.cfg.MaxCallsPerHour <= 0 {
		return true
	}
	w.limitMu.Lock()
	defer w.limitMu.Unlock()
	cutoff := now.Add(-time.Hour)
	pruned := w.tokens[:0]
	for _, t := range w.tokens {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	w.tokens = pruned
	if len(w.tokens) >= w.cfg.MaxCallsPerHour {
		return false
	}
	w.tokens = append(w.tokens, now)
	return true
}

// releaseRateLimit removes the most recent token. Callers run it
// when a candidate rejection happens after consumeRateLimit
// succeeded so a wasted token does not count against the budget.
func (w *Worker) releaseRateLimit() {
	w.limitMu.Lock()
	defer w.limitMu.Unlock()
	if len(w.tokens) == 0 {
		return
	}
	w.tokens = w.tokens[:len(w.tokens)-1]
}
