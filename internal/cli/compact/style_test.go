package compact

import (
	"strings"
	"testing"
	"time"

	compactengine "goodkind.io/clyde/internal/compact"
)

func TestPhaseFromStep_ToolsPassLabels(t *testing.T) {
	got := phaseFromStep("tools full -> line-only (oldest 32)")
	if got != "tools pass 1/2 (full -> line-only)" {
		t.Fatalf("phaseFromStep pass1 = %q", got)
	}

	got = phaseFromStep("tools line-only -> drop (oldest 32)")
	if got != "tools pass 2/2 (line-only -> drop)" {
		t.Fatalf("phaseFromStep pass2 = %q", got)
	}
}

func TestComposePanelLines_CompletedShowsUpfrontCurrent(t *testing.T) {
	// On completion the Current row must reflect the live /context probe
	// value captured upfront, not the planner projection
	// (static_floor + final_tail + reserved). The planner projection
	// drifts from claude's /context reporter; the upfront probe value is
	// the same number /context would render inside the chat.
	const upfrontCurrent = 412345
	const staticFloor = 70703
	const reserved = 13000
	const finalTail = 100
	p := &progressView{
		target: 200000,
		mode:   ModePreview,
		upfront: UpfrontStats{
			StaticFloor:  staticFloor,
			Reserved:     reserved,
			CurrentTotal: upfrontCurrent,
		},
		startedAt:     time.Now().Add(-3 * time.Second),
		completed:     true,
		finalStatic:   staticFloor,
		finalReserved: reserved,
		finalRes: &compactengine.PlanResult{
			FinalTail: finalTail,
		},
	}
	rec := compactengine.IterationRecord{
		Step:     "done",
		CtxTotal: 999999, // intentionally not the value we want to render
	}

	lines := p.composePanelLines("⠙", 3*time.Second, rec, rec.Step, humanInt(rec.CtxTotal))
	joined := strings.Join(lines, "\n")

	wantCurrent := humanInt(upfrontCurrent)
	if !strings.Contains(joined, wantCurrent) {
		t.Fatalf("completed panel Current should show upfront value %q\n%s", wantCurrent, joined)
	}

	projection := humanInt(staticFloor + finalTail + reserved)
	// Render-only sanity: the old projection number should not appear
	// where the Current row sits. Use a Current-row substring to scope
	// the check so we do not accidentally trip on the same number
	// appearing in an unrelated row in future renderers.
	for _, line := range lines {
		if !strings.Contains(line, "Current") {
			continue
		}
		if strings.Contains(line, projection) {
			t.Fatalf("Current row should not show planner projection %q: %q", projection, line)
		}
	}
}

func TestComposePanelLines_LabelFirstLayout(t *testing.T) {
	p := &progressView{
		target: 200000,
		mode:   ModePreview,
		upfront: UpfrontStats{
			StaticFloor: 70703,
			Reserved:    13000,
		},
		startedAt: time.Now().Add(-3 * time.Second),
	}
	rec := compactengine.IterationRecord{
		Step:              "tools line-only -> drop (oldest 8)",
		TailTokens:        116297,
		CtxTotal:          200000,
		Delta:             0,
		ThinkingDropped:   true,
		ImagesPlaceholder: true,
		ToolsDropped:      10,
		ChatTurnsTotal:    20,
	}

	lines := p.composePanelLines("⠙", 3*time.Second, rec, rec.Step, humanInt(rec.CtxTotal))
	joined := strings.Join(lines, "\n")

	wantSubstrings := []string{
		"Current",
		"Target",
		"Trimmed",
		"thinking",
		"images",
		"chat",
		"tools",
		"tools pass 2/2 (line-only -> drop)",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Fatalf("panel missing %q\n%s", want, joined)
		}
	}

	rejectSubstrings := []string{
		"token math",
		"always-kept",
		"message budget",
		"safety buffer",
		"static tokens",
		"equation",
		"over/under target",
		"tail",
	}
	for _, reject := range rejectSubstrings {
		if strings.Contains(joined, reject) {
			t.Fatalf("panel unexpectedly contains stale wording %q\n%s", reject, joined)
		}
	}
}
