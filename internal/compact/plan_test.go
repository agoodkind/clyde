package compact

import (
	"context"
	"fmt"
	"testing"
	"time"

	"goodkind.io/clyde/internal/contextcount"
)

// toolPairLines builds n assistant(tool_use)+user(tool_result) pairs in
// a single parent chain, with no compact boundary so the whole list is
// the post-boundary slice.
func toolPairLines(n int, t0 time.Time) []string {
	var lines []string
	parent := ""
	for i := 0; i < n; i++ {
		aID := fmt.Sprintf("a%d", i)
		uID := fmt.Sprintf("u%d", i)
		toolID := fmt.Sprintf("tool_%d", i)
		lines = append(lines, assistantBlocks(aID, parent, t0.Add(time.Duration(2*i)*time.Second), []map[string]any{
			{"type": "tool_use", "id": toolID, "name": "Bash", "input": map[string]any{"cmd": "ls"}},
		}))
		lines = append(lines, userBlocks(uID, aID, t0.Add(time.Duration(2*i+1)*time.Second), []map[string]any{
			{"type": "tool_result", "tool_use_id": toolID, "content": "tool output body"},
		}))
		parent = uID
	}
	return lines
}

// fullToolBlockCounter charges 100 tokens for every surviving full
// tool_use or tool_result block in the transcript. Demoting a pair to
// line-only or dropping it turns those blocks into text, so the count
// falls by 200 per demoted pair. The returned closure plus base lets a
// test place the floor above or below the target.
func fullToolBlockCounter(base int) func(int32, contextcount.Transcript) int {
	return func(_ int32, tr contextcount.Transcript) int {
		full := 0
		for _, message := range tr.Messages {
			for _, block := range message.Content {
				if block.Type == "tool_use" || block.Type == "tool_result" {
					full++
				}
			}
		}
		return base + full*100
	}
}

// TestRunToolDemotionsTerminatesOnOvershoot reproduces the tool-pass
// non-termination bug. With 4 pairs the baseline is 800 tokens and
// dropping every pair reaches 0, so the tool axis crosses under the
// 500 target. The old batch loop reverted the overshoot and re-measured
// the same batch forever. The bisect must converge to the smallest
// result at or above target and issue a bounded number of counter
// calls.
func TestRunToolDemotionsTerminatesOnOvershoot(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	slice, err := LoadSlice(writeTranscript(t, toolPairLines(4, t0)))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	fake := &fakeContextCounter{tokens: fullToolBlockCounter(0), source: contextcount.CounterSourceAPI}

	type outcome struct {
		res *PlanResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, runErr := RunPlan(context.Background(), PlanInput{
			Slice:          slice,
			Strippers:      Strippers{Tools: true},
			Target:         500,
			StaticOverhead: 0,
			Reserved:       0,
			Counter:        fake,
		})
		done <- outcome{res: res, err: runErr}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("RunPlan: %v", got.err)
		}
		last := got.res.Iterations[len(got.res.Iterations)-1]
		if last.CtxTotal < 500 {
			t.Fatalf("result %d is under target 500 (over-trimmed)", last.CtxTotal)
		}
		if last.CtxTotal != 600 {
			t.Fatalf("result = %d, want 600 (smallest reachable at or above target)", last.CtxTotal)
		}
		if !got.res.HitTarget {
			t.Fatalf("HitTarget = false, want true (planner stopped one drop short of undershooting)")
		}
		if calls := fake.calls.Load(); calls > 40 {
			t.Fatalf("counter calls = %d, want bounded (<=40); the loop is not converging", calls)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunPlan did not terminate: the tool demotion pass did not converge")
	}
}

// TestRunPlanCommitsFloorAboveTarget covers the case where the floor
// sits above target. With base 600 even dropping every tool stays at
// 600, above the 500 target. The planner must commit that result
// (valid, at or above target), report HitTarget false to signal the
// floor is above target, and not loop.
func TestRunPlanCommitsFloorAboveTarget(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	slice, err := LoadSlice(writeTranscript(t, toolPairLines(4, t0)))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	fake := &fakeContextCounter{tokens: fullToolBlockCounter(600), source: contextcount.CounterSourceAPI}

	res, err := RunPlan(context.Background(), PlanInput{
		Slice:          slice,
		Strippers:      Strippers{Tools: true},
		Target:         500,
		StaticOverhead: 0,
		Reserved:       0,
		Counter:        fake,
	})
	if err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	last := res.Iterations[len(res.Iterations)-1]
	if last.CtxTotal < 500 {
		t.Fatalf("result %d is under target 500 (over-trimmed)", last.CtxTotal)
	}
	if res.HitTarget {
		t.Fatalf("HitTarget = true, want false (floor sits above target)")
	}
}
