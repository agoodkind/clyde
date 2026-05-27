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

// TestComposePanelLines_ProbingShowsLoadingPlaceholders verifies that
// while the daemon's /context probe is in flight the running panel
// renders "loading..." for the Current row and every Trimmed category
// rather than "?" or zero values. The panel mirrors the session-details
// pane so the user sees an immediate spinner+loading-row view instead
// of an empty terminal for the 4-7 seconds the probe takes.
func TestComposePanelLines_ProbingShowsLoadingPlaceholders(t *testing.T) {
	p := &progressView{
		target:        200000,
		mode:          ModePreview,
		startedAt:     time.Now().Add(-1 * time.Second),
		probing:       true,
		statusMessage: "loading transcript and probing context...",
	}
	rec := compactengine.IterationRecord{
		Step:     "",
		CtxTotal: 0,
	}
	// Pass "?" as the synthetic ctxStr the draw loop would compute when
	// rec.CtxTotal is zero; the probing branch must override it.
	lines := p.composePanelLines("⠙", 1*time.Second, rec, "", "?")
	joined := strings.Join(lines, "\n")

	wantContains := []string{
		"loading...",
		"probing context",
	}
	for _, want := range wantContains {
		if !strings.Contains(joined, want) {
			t.Fatalf("probing panel missing %q\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Current") {
		// Sanity: the Current row exists. Its value must read loading...
		// rather than "?" or "0".
		for _, line := range lines {
			if !strings.Contains(line, "Current") {
				continue
			}
			if strings.Contains(line, "?") {
				t.Fatalf("Current row should not show '?' while probing: %q", line)
			}
			if !strings.Contains(line, "loading...") {
				t.Fatalf("Current row should show loading...: %q", line)
			}
		}
	}
}

// TestComposePanelLines_ClearProbingShowsLiveValues verifies that the
// running panel transitions from "loading..." back to live numbers
// after the upfront event lands.
func TestComposePanelLines_ClearProbingShowsLiveValues(t *testing.T) {
	p := &progressView{
		target:        200000,
		mode:          ModePreview,
		startedAt:     time.Now().Add(-1 * time.Second),
		probing:       true,
		statusMessage: "probing",
	}
	upfront := UpfrontStats{
		SessionName:   "demo",
		Mode:          ModePreview,
		CurrentTotal:  412345,
		MaxTokens:     1000000,
		Target:        200000,
		StrippersText: "thinking,images,tools,chat",
	}
	// Simulate the upfront event arriving by clearing probing in place.
	p.probing = false
	p.statusMessage = ""
	p.upfront = upfront
	rec := compactengine.IterationRecord{
		Step:     "baseline",
		CtxTotal: 412345,
	}
	lines := p.composePanelLines("⠙", 2*time.Second, rec, "baseline", humanInt(rec.CtxTotal))
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "loading...") {
		t.Fatalf("post-probe panel should not show loading placeholders:\n%s", joined)
	}
	wantCurrent := humanInt(upfront.CurrentTotal)
	if !strings.Contains(joined, wantCurrent) {
		t.Fatalf("post-probe panel should show live Current %q\n%s", wantCurrent, joined)
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
