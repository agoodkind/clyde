// Package clock is the single permitted home for direct wall-time
// calls. Every other package reads time through the package-level
// [Now] and [Since] helpers instead of importing [time.Now] or
// [time.Since] directly. Tests substitute the wall clock through [Set].
// The staticcheck-extra `time_now_outside_clock` analyzer enforces
// this so production call sites stay testable.
package clock

import "time"

// Clock abstracts wall-time reads so tests can substitute a fake
// implementation without touching production call sites.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

// Wall is the production clock. It forwards to the stdlib time
// package and is the only call site in the repo permitted to do so.
type Wall struct{}

// Now returns the current wall time.
func (Wall) Now() time.Time { return time.Now() }

// Since returns the duration elapsed since t, computed against the
// current wall time.
func (Wall) Since(t time.Time) time.Duration { return time.Since(t) }

// current is the process-wide clock backing the package-level
// [Now] and [Since] helpers. Production code reads through Now and
// Since; tests substitute the clock through file-private helpers in
// the same package they exercise.
var current Clock = Wall{}

// Now returns the current wall time from the active clock.
func Now() time.Time { return current.Now() }

// Since returns the duration elapsed since t against the active clock.
func Since(t time.Time) time.Duration { return current.Since(t) }
