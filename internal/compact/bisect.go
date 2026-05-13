package compact

import (
	"context"
	"fmt"
)

// Axis describes one dimension along which the planner searches for
// the smallest amount of removal that brings the projected context
// total at or below Target. The search space is integers in [0, N].
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
	// returned k is the smallest where Probe <= Target.
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

// BisectMin returns the minimum-disruption k in [0, N] for the
// conservative policy "stop just before undershooting Target." The
// returned k satisfies Probe(k) >= Target whenever any such k <= N
// exists; otherwise k=N. The caller applies the first k steps of the
// underlying drop list. This matches the prior linear-scan policy of
// preferring to leave ctx slightly over target rather than cross
// under, which keeps the planner from dropping more content than is
// strictly necessary to approach the target.
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

	// Boundary probe at k=N. If the floor is still at or above
	// target, applying every drop still does not undershoot, so the
	// answer is N. Surface that and let the caller decide whether to
	// accept N or refuse the run.
	totalAtN, err := axis.Probe(ctx, axis.N)
	if err != nil {
		return 0, err
	}
	emit(axis.N, totalAtN)
	if totalAtN >= axis.Target {
		return axis.N, nil
	}

	// Invariant from here: Probe(lo) >= target, Probe(hi) < target.
	// lo=0 by precondition (caller guarantees baseline over-target,
	// strictly). hi=N from the boundary probe (strictly under target).
	// Bisect for the largest k where Probe(k) >= target.
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
