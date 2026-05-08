package sessionrename

import (
	"time"

	"goodkind.io/clyde/internal/session"
)

// GateResult is the outcome of evaluating the trigger gates against
// a candidate session. Reason is the machine-readable rejection
// cause when Allow is false.
type GateResult struct {
	Allow  bool
	Reason string
}

// allow is the canonical "let it through" GateResult.
func allow() GateResult { return GateResult{Allow: true, Reason: ""} }

// deny attaches a structured reason to a rejected gate evaluation.
func deny(reason string) GateResult { return GateResult{Allow: false, Reason: reason} }

// EvaluateGates returns Allow=false with a Reason whenever any of
// the rules below would prevent the worker from proposing a new
// name. Daemon callers run this before reading the transcript so a
// session that fails a gate never costs an [os.Open].
//
// The gates close finding-1 through finding-4 from the post-mortem:
//   - user-owned predicate locks user-typed names.
//   - cooldown caps the daemon-wide retry rate.
//   - live-lease check defers rename while a wrapper holds the
//     session.
//   - minimum user message count keeps the worker out of cold
//     sessions whose first message has not arrived.
//
// The candidate-source hash gate lives at the worker level; this
// helper is intentionally cheap and side-effect free.
func EvaluateGates(sess *session.Session, userMsgCount int, leaseHeld bool, now time.Time, cfg GatesConfig) GateResult {
	if sess == nil {
		return deny("nil_session")
	}
	if leaseHeld {
		return deny("live_lease_held")
	}
	if sess.Metadata.AutoNameSource == session.AutoNameSourceUser {
		return deny("user_owned")
	}
	if sess.Metadata.AutoNameState == session.AutoNameStateUserLocked {
		return deny("user_locked")
	}
	minMsgs := cfg.MinUserMessages
	if minMsgs <= 0 {
		minMsgs = 3
	}
	if userMsgCount < minMsgs {
		return deny("min_user_messages")
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = 30 * time.Minute
	}
	if !sess.Metadata.LastAutoNameAt.IsZero() && now.Sub(sess.Metadata.LastAutoNameAt) < cooldown {
		return deny("cooldown_active")
	}
	return allow()
}

// GatesConfig holds the tunables EvaluateGates consults. PR3 lands
// the user-facing AutoNameConfig; the worker owns the conversion to
// this internal shape so the config package does not become a
// transitive dependency of every test.
type GatesConfig struct {
	MinUserMessages int
	Cooldown        time.Duration
}
