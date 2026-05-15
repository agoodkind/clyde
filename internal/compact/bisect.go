package compact

import (
	"context"
	"fmt"
)

// Axis describes one dimension along which the planner searches for
// the largest amount of removal that keeps the projected context total
// greater than or equal to Target. The search space is integers in [0, N].
// Probe owns its own state: it applies candidate k, measures the
// resulting context total, restores the prior state, and returns the
// measurement. The bisect never touches caller state directly.
//
// Implementations must satisfy a monotone-non-increasing invariant on
// Probe as k grows: dropping more is never worse for the projection.
// The bisect logic relies on this invariant to halve the search space
// every probe.
type Axis struct {
	// N is the upper bound of the search space, inclusive. k=0 means
	// "no removal" (baseline). k=N means "remove everything droppable".
	N int

	// Probe mutates the caller's state to represent candidate k,
	// measures the resulting projected context total, restores the
	// state to baseline, and returns the measurement. Errors short
	// the bisect.
	Probe func(ctx context.Context, k int) (ctxTotal int, err error)

	// Target is the threshold the bisect is searching against. The
	// returned k is the largest candidate where Probe >= Target.
	Target int

	// Label produces a human-readable description for probe rows.
	Label func(k int) string

	// Emit posts a probe row to the iteration log so the dashboard
	// can render mid-bisect measurements. Pass nil to skip.
	Emit func(IterationRecord)

	// BuildRecord turns a probe measurement into an IterationRecord
	// the planner can emit. The caller fills in category counts and
	// other fields from its surrounding state.
	BuildRecord func(label string, k int, ctxTotal int) IterationRecord
}

// BisectMin returns the largest k in [0, N] where Probe(k) is still at
// or above Target. The caller applies the first k steps of the
// underlying drop list. The returned k leaves the projection slightly
// over target rather than crossing under, so the operator gets the
// least-disruptive Apply that respects the user's stated headroom. If
// even k=N still leaves Probe at or above Target, BisectMin returns N
// so the caller applies every available drop.
//
// The caller must guarantee Probe(0) > Target before calling; if
// baseline already satisfies the target there is no work to do and
// the planner should not call BisectMin.
//
// Probes used: one boundary probe at k=N plus ceil(log2(N)) halving
// probes in the worst case.
func BisectMin(ctx context.Context, axis Axis) (int, error) {
	if axis.N <= 0 {
		return 0, nil
	}
	if axis.Probe == nil {
		return 0, fmt.Errorf("bisect: axis Probe is nil")
	}

	emit := func(k int, total int) {
		if axis.Emit == nil || axis.BuildRecord == nil {
			return
		}
		label := ""
		if axis.Label != nil {
			label = axis.Label(k)
		}
		axis.Emit(axis.BuildRecord(label, k, total))
	}

	// Boundary probe at k=N. If the floor is still at or above target,
	// applying every drop still does not undershoot, so the answer is
	// N. The caller applies every available drop in that case.
	totalAtN, err := axis.Probe(ctx, axis.N)
	if err != nil {
		return 0, err
	}
	emit(axis.N, totalAtN)
	if totalAtN >= axis.Target {
		return axis.N, nil
	}

	// Invariant from here: Probe(lo) >= target, Probe(hi) < target.
	// lo=0 by precondition. hi=N from the boundary probe. Bisect for
	// the largest k where Probe(k) >= target.
	lo, hi := 0, axis.N
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		total, err := axis.Probe(ctx, mid)
		if err != nil {
			return 0, err
		}
		emit(mid, total)
		if total >= axis.Target {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}
