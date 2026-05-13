package compact

import (
	"context"
	"math"
	"testing"
)

// stubAxis builds an Axis with a deterministic monotone-non-increasing
// measurement function and a probe counter so tests can assert
// convergence cost without rebuilding wiring each case.
func stubAxis(n int, target int, measureAt func(k int) int) (*Axis, *[]int) {
	probes := make([]int, 0, n)
	axis := Axis{
		N: n,
		Probe: func(ctx context.Context, k int) (int, error) {
			probes = append(probes, k)
			return measureAt(k), nil
		},
		Target:      target,
		Label:       func(k int) string { return "probe" },
		Emit:        nil,
		BuildRecord: nil,
	}
	return &axis, &probes
}

func TestBisectMin_ReturnsSmallestKAtOrUnderTarget(t *testing.T) {
	// Measure(k) = 100 - k. Target 50. The smallest k where Probe is
	// less than or equal to 50 is k=50.
	axis, probes := stubAxis(100, 50, func(k int) int { return 100 - k })
	got, err := BisectMin(context.Background(), *axis)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 50 {
		t.Fatalf("got k=%d want 50", got)
	}
	maxProbes := int(math.Ceil(math.Log2(100))) + 2
	if len(*probes) > maxProbes {
		t.Fatalf("probe count %d exceeds budget %d (probes=%v)", len(*probes), maxProbes, *probes)
	}
}

func TestBisectMin_FloorStillOverTarget(t *testing.T) {
	// Probe(N=100) = 100 > target 50. Cannot reach target, so return N.
	axis, _ := stubAxis(100, 50, func(k int) int { return 200 - k })
	got, err := BisectMin(context.Background(), *axis)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 100 {
		t.Fatalf("got k=%d want 100 (the cap)", got)
	}
}

func TestBisectMin_ZeroN(t *testing.T) {
	axis, _ := stubAxis(0, 50, func(k int) int { return 100 })
	got, err := BisectMin(context.Background(), *axis)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Fatalf("got k=%d want 0 for empty search space", got)
	}
}

func TestBisectMin_ReturnsFirstStepThatReachesTarget(t *testing.T) {
	// Step function: Probe drops below target only at k >= 17. The
	// smallest k where Probe <= 10 is k=17.
	axis, _ := stubAxis(64, 10, func(k int) int {
		if k >= 17 {
			return 5
		}
		return 100
	})
	got, err := BisectMin(context.Background(), *axis)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 17 {
		t.Fatalf("got k=%d want 17 (first k under target)", got)
	}
}

func TestBisectMin_EmitsProbeRows(t *testing.T) {
	emitted := []IterationRecord{}
	axis := Axis{
		N: 8,
		Probe: func(ctx context.Context, k int) (int, error) {
			return 100 - k*20, nil
		},
		Target: 50,
		Label:  func(k int) string { return "test" },
		Emit:   func(r IterationRecord) { emitted = append(emitted, r) },
		BuildRecord: func(label string, k int, ctxTotal int) IterationRecord {
			return IterationRecord{Step: label, CtxTotal: ctxTotal}
		},
	}
	_, err := BisectMin(context.Background(), axis)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(emitted) == 0 {
		t.Fatalf("expected probe rows to be emitted")
	}
}
