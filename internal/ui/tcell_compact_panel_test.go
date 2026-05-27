package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestCompactPanelSliderAdjustsTarget(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.maxTokens = 1000000
	panel.targetTokens = 200000
	panel.targetText = "200000"
	panel.focusGroup = 0

	handled := panel.HandleEvent(tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone))
	if !handled {
		t.Fatalf("expected slider key to be handled")
	}
	if panel.targetTokens >= 200000 {
		t.Fatalf("expected right arrow to shrink target, got %d", panel.targetTokens)
	}

	handled = panel.HandleEvent(tcell.NewEventKey(tcell.KeyLeft, 0, tcell.ModNone))
	if !handled {
		t.Fatalf("expected slider key to be handled")
	}
	if panel.targetTokens <= 190000 {
		t.Fatalf("expected left arrow to grow target, got %d", panel.targetTokens)
	}
}

func TestCompactPanelTargetInputParsesDigits(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.maxTokens = 1000000
	panel.targetText = ""
	panel.focusGroup = 1

	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyRune, '3', tcell.ModNone))
	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyRune, '0', tcell.ModNone))
	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyRune, '0', tcell.ModNone))

	if panel.targetTokens != 300 {
		t.Fatalf("expected target 300, got %d", panel.targetTokens)
	}
}

func TestCompactPanelPreviewShortcutCallsCallback(t *testing.T) {
	panel := NewCompactPanel("demo")
	called := false
	panel.OnPreview = func(req CompactRunRequest) {
		called = true
		if req.TargetTokens <= 0 {
			t.Fatalf("expected target tokens in request")
		}
	}

	handled := panel.HandleEvent(tcell.NewEventKey(tcell.KeyRune, 'p', tcell.ModNone))
	if !handled {
		t.Fatalf("expected preview shortcut to be handled")
	}
	if !called {
		t.Fatalf("expected preview callback to run")
	}
}

func TestCompactPanelArrowFocusNavigation(t *testing.T) {
	panel := NewCompactPanel("demo")

	handled := panel.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if !handled {
		t.Fatalf("expected down key to move focus")
	}
	if panel.focusGroup != 1 {
		t.Fatalf("expected focus group 1 after down, got %d", panel.focusGroup)
	}

	handled = panel.HandleEvent(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if !handled {
		t.Fatalf("expected up key to move focus")
	}
	if panel.focusGroup != 0 {
		t.Fatalf("expected focus group 0 after up, got %d", panel.focusGroup)
	}
}

func TestCompactPanelTabDoesNotChangeFocus(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.focusGroup = 2

	handled := panel.HandleEvent(tcell.NewEventKey(tcell.KeyTab, 0, tcell.ModNone))
	if handled {
		t.Fatalf("expected tab key to be ignored")
	}
	if panel.focusGroup != 2 {
		t.Fatalf("expected focus group unchanged, got %d", panel.focusGroup)
	}
}

func TestCompactPanelApplyRequiresConfirmation(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.focusGroup = 3
	panel.actionIdx = 1
	applied := false
	panel.OnApply = func(req CompactRunRequest) {
		applied = true
	}

	handled := panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if !handled {
		t.Fatalf("expected enter to be handled on actions row")
	}
	if applied {
		t.Fatalf("expected first enter to arm confirmation, not apply")
	}
	if !panel.confirmApply {
		t.Fatalf("expected apply confirmation to be armed")
	}

	handled = panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if !handled {
		t.Fatalf("expected second enter to be handled")
	}
	if !applied {
		t.Fatalf("expected second enter to apply after confirmation")
	}
}

func TestCompactPanelApplyRequiresConfirmationWithSpace(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.focusGroup = 3
	panel.actionIdx = 1
	applied := false
	panel.OnApply = func(req CompactRunRequest) {
		applied = true
	}

	handled := panel.HandleEvent(tcell.NewEventKey(tcell.KeyRune, ' ', tcell.ModNone))
	if !handled {
		t.Fatalf("expected space to be handled on actions row")
	}
	if applied {
		t.Fatalf("expected first space to arm confirmation, not apply")
	}
	if !panel.confirmApply {
		t.Fatalf("expected apply confirmation to be armed")
	}

	handled = panel.HandleEvent(tcell.NewEventKey(tcell.KeyRune, ' ', tcell.ModNone))
	if !handled {
		t.Fatalf("expected second space to be handled")
	}
	if !applied {
		t.Fatalf("expected second space to apply after confirmation")
	}
}

func TestCompactPanelEnterOutsideActionsDoesNotApply(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.focusGroup = 1
	applied := false
	panel.OnApply = func(req CompactRunRequest) {
		applied = true
	}

	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if applied {
		t.Fatalf("expected enter outside actions to not apply")
	}
}

func TestCompactPanelCheckboxEnterToggles(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.focusGroup = 2
	panel.checkboxIdx = 0
	panel.thinking = true

	handled := panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if !handled {
		t.Fatalf("expected enter to toggle focused checkbox")
	}
	if panel.thinking {
		t.Fatalf("expected thinking checkbox to toggle off")
	}
}

func TestCompactPanelSummaryControlCyclesModes(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.focusGroup = 2
	panel.checkboxIdx = 4

	if panel.summary != "auto" {
		t.Fatalf("summary mode = %q, want auto", panel.summary)
	}
	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if panel.summary != "on" {
		t.Fatalf("summary mode = %q, want on", panel.summary)
	}
	req := panel.buildRequest()
	if req.SummarizeMode != "on" || !req.Summarize {
		t.Fatalf("request summary = mode:%q bool:%v, want on/true", req.SummarizeMode, req.Summarize)
	}
	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if panel.summary != "off" {
		t.Fatalf("summary mode = %q, want off", panel.summary)
	}
	req = panel.buildRequest()
	if req.SummarizeMode != "off" || req.Summarize {
		t.Fatalf("request summary = mode:%q bool:%v, want off/false", req.SummarizeMode, req.Summarize)
	}
	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if panel.summary != "auto" {
		t.Fatalf("summary mode = %q, want auto", panel.summary)
	}
}

func TestPercentLabelUsesCommaGrouping(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.maxTokens = 1000000
	panel.targetTokens = 200000

	label := panel.percentLabel()
	if label != "20% (200,000/1,000,000)" {
		t.Fatalf("unexpected percent label: %q", label)
	}
}

func TestCompactPanelSliderFillRepresentsCompactedShare(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.maxTokens = 1000000
	panel.targetTokens = 200000

	got := panel.renderSlider(20)
	want := "|================----|"
	if got != want {
		t.Fatalf("slider = %q want %q", got, want)
	}
}

func TestCompactPanelStatusLegendUsesEnumActions(t *testing.T) {
	panel := NewCompactPanel("demo")
	actions := panel.StatusLegendActions()
	if len(actions) == 0 {
		t.Fatalf("expected compact panel legend actions")
	}
	hasSelect := slices.Contains(actions, LegendSelect)
	if !hasSelect {
		t.Fatalf("expected compact panel legend actions to include LegendSelect")
	}
	for _, action := range actions {
		if action == LegendPreview || action == LegendApply || action == LegendUndo {
			t.Fatalf("expected compact panel legend actions to stay succinct, got action %v", action)
		}
	}
}

func TestCompactPanelBusyDrawsDisabledActionButtons(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(90, 24)

	panel := NewCompactPanel("demo")
	panel.focusGroup = 3
	panel.actionIdx = 1
	panel.SetBusy("apply", true)
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 90, H: 24})
	scr.Show()

	x, y, ok := findStringCell(scr, "Apply")
	if !ok {
		t.Fatalf("expected Apply button to be drawn")
	}
	_, style, _ := scr.Get(x, y)
	fg, bg, _ := style.Decompose()
	if bg == ColorSelected {
		t.Fatalf("expected busy Apply button to not use selected background")
	}
	if fg != ColorMuted {
		t.Fatalf("expected busy Apply button to be muted, fg=%v", fg)
	}
}

func TestCompactPanelIdleProgressDoesNotShowWaitingSpinner(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(90, 24)

	panel := NewCompactPanel("demo")
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 90, H: 24})
	scr.Show()

	text := compactPanelScreenText(scr)
	if strings.Contains(text, "waiting for progress") {
		t.Fatalf("idle compact panel should not show waiting spinner:\n%s", text)
	}
	if !strings.Contains(text, "No run yet") {
		t.Fatalf("idle compact panel missing static hint:\n%s", text)
	}
}

func TestCompactPanelBusyProgressShowsWaitingSpinner(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(90, 24)

	panel := NewCompactPanel("demo")
	panel.SetBusy("preview", true)
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 90, H: 24})
	scr.Show()

	text := compactPanelScreenText(scr)
	if !strings.Contains(text, "preview in progress") {
		t.Fatalf("busy compact panel should show waiting spinner:\n%s", text)
	}
}

func TestCompactPanelBusyProgressShowsSpinnerAfterIterationLines(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(90, 24)

	panel := NewCompactPanel("demo")
	panel.ApplyCompactEvent(CompactEvent{
		Kind: "iteration",
		Iteration: &CompactIteration{
			Iteration: 1,
			Step:      "baseline",
			CtxTotal:  900000,
			Delta:     700000,
		},
	})
	panel.SetBusy("apply", true)
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 90, H: 24})
	scr.Show()

	text := compactPanelScreenText(scr)
	if !strings.Contains(text, "iter 1") {
		t.Fatalf("busy compact panel should keep prior iteration output:\n%s", text)
	}
	if !strings.Contains(text, "projected 900,000") {
		t.Fatalf("busy compact panel should label iteration totals as projected:\n%s", text)
	}
	if !strings.Contains(text, "apply in progress") {
		t.Fatalf("busy compact panel should keep animating after progress exists:\n%s", text)
	}
}

func TestCompactPanelInitialContextUsageShowsLoadingStatus(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(100, 28)

	panel := NewCompactPanel("demo")
	panel.setInitialContextUsage(SessionContextUsage{
		Model:          "",
		TotalTokens:    0,
		MaxTokens:      0,
		Percentage:     0,
		MessagesTokens: 0,
	}, false, "loading...")
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 100, H: 28})
	scr.Show()

	text := compactPanelScreenText(scr)
	if !strings.Contains(text, "current    loading...") {
		t.Fatalf("initial compact panel should show context loading status:\n%s", text)
	}
	if !strings.Contains(text, "images          preview required") {
		t.Fatalf("initial compact panel should explain count details need preview:\n%s", text)
	}
}

func TestCompactPanelInitialContextUsageShowsCachedContext(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(100, 28)

	panel := NewCompactPanel("demo")
	panel.setInitialContextUsage(SessionContextUsage{
		Model:          "opus",
		TotalTokens:    123456,
		MaxTokens:      1000000,
		Percentage:     12,
		MessagesTokens: 98765,
	}, true, "")
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 100, H: 28})
	scr.Show()

	text := compactPanelScreenText(scr)
	if !strings.Contains(text, "session demo  model opus") {
		t.Fatalf("initial compact panel should use cached context model:\n%s", text)
	}
	if !strings.Contains(text, "current    123,456 / 1,000,000") {
		t.Fatalf("initial compact panel should show cached current context:\n%s", text)
	}
	if !strings.Contains(text, "messages   98,765") {
		t.Fatalf("initial compact panel should show cached message context:\n%s", text)
	}
}

func TestCompactPanelProgressLogStaysAboveActions(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(90, 18)

	panel := NewCompactPanel("demo")
	for i := 1; i <= 12; i++ {
		panel.ApplyCompactEvent(CompactEvent{
			Kind: "iteration",
			Iteration: &CompactIteration{
				Iteration: i,
				Step:      fmt.Sprintf("step-%02d", i),
				CtxTotal:  900000 - i,
				Delta:     -i,
			},
		})
	}
	panel.ApplyCompactEvent(CompactEvent{
		Kind:  "final",
		Final: &CompactFinal{FinalTail: 100, TargetTokens: 300000, StaticFloor: 200, ReservedTokens: 300},
	})
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 90, H: 18})
	scr.Show()

	text := compactPanelScreenText(scr)
	actionIdx := strings.Index(text, "Actions")
	if actionIdx < 0 {
		t.Fatalf("expected Actions row in render:\n%s", text)
	}
	if !strings.Contains(text, "Progress") {
		t.Fatalf("expected progress log box title in render:\n%s", text)
	}
	afterActions := text[actionIdx:]
	if strings.Contains(afterActions, "final current") || strings.Contains(afterActions, "step-") {
		t.Fatalf("progress log leaked into action rows:\n%s", afterActions)
	}
}

func TestCompactPanelMouseWheelScrollsProgressLog(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(90, 20)

	panel := NewCompactPanel("demo")
	for i := 1; i <= 20; i++ {
		panel.ApplyCompactEvent(CompactEvent{
			Kind: "iteration",
			Iteration: &CompactIteration{
				Iteration: i,
				Step:      fmt.Sprintf("step-%02d", i),
				CtxTotal:  900000 - i,
				Delta:     -i,
			},
		})
	}
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 90, H: 20})
	scr.Show()
	if panel.logRect.W == 0 || panel.logRect.H == 0 {
		t.Fatalf("expected progress log rect to be tracked")
	}

	x := panel.logRect.X + 1
	y := panel.logRect.Y + panel.logRect.H/2
	handled := panel.HandleEvent(tcell.NewEventMouse(x, y, tcell.WheelUp, tcell.ModNone))
	if !handled {
		t.Fatalf("expected wheel over progress log to be handled")
	}
	if panel.logScroll <= 0 {
		t.Fatalf("expected wheel up to scroll toward older progress lines")
	}

	handled = panel.HandleEvent(tcell.NewEventMouse(x, y, tcell.WheelDown, tcell.ModNone))
	if !handled {
		t.Fatalf("expected wheel down over progress log to be handled")
	}
	if panel.logScroll != 0 {
		t.Fatalf("expected wheel down to return toward newest progress lines, got %d", panel.logScroll)
	}
}

func TestCompactPanelApplyCompactEventTracksIterationHistory(t *testing.T) {
	panel := NewCompactPanel("demo")
	panel.ApplyCompactEvent(CompactEvent{
		Kind: "upfront",
		Upfront: &CompactUpfront{
			SessionName: "demo",
			SessionID:   "sess-1",
			Model:       "opus",
		},
	})
	panel.ApplyCompactEvent(CompactEvent{
		Kind: "iteration",
		Iteration: &CompactIteration{
			Iteration: 1,
			Step:      "baseline",
			CtxTotal:  900000,
			Delta:     400000,
		},
	})
	panel.ApplyCompactEvent(CompactEvent{
		Kind: "iteration",
		Iteration: &CompactIteration{
			Iteration: 2,
			Step:      "drop chat",
			CtxTotal:  500100,
			Delta:     100,
		},
	})

	if panel.latestIteration == nil || panel.latestIteration.Iteration != 2 {
		t.Fatalf("expected latest iteration 2, got %#v", panel.latestIteration)
	}
	if got := len(panel.iterationHistory); got != 2 {
		t.Fatalf("expected 2 history rows, got %d", got)
	}
}

func findStringCell(scr tcell.SimulationScreen, target string) (int, int, bool) {
	cells, width, height := scr.GetContents()
	for y := range height {
		var row strings.Builder
		for x := range width {
			cell := cells[y*width+x]
			if len(cell.Runes) == 0 || cell.Runes[0] == 0 {
				row.WriteRune(' ')
				continue
			}
			row.WriteRune(cell.Runes[0])
		}
		if x := strings.Index(row.String(), target); x >= 0 {
			return x, y, true
		}
	}
	return 0, 0, false
}

func compactPanelScreenText(scr tcell.SimulationScreen) string {
	cells, width, height := scr.GetContents()
	var b strings.Builder
	for y := range height {
		for x := range width {
			cell := cells[y*width+x]
			if len(cell.Runes) == 0 || cell.Runes[0] == 0 {
				b.WriteRune(' ')
				continue
			}
			b.WriteRune(cell.Runes[0])
		}
		b.WriteByte('\n')
	}
	return b.String()
}
