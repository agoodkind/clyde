package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestLegendForCompactNoLongerUsesTabHint(t *testing.T) {
	segs := legendForStatus(&StatusBarWidget{Mode: StatusCompact, DaemonOnline: true})
	var joined strings.Builder
	for _, seg := range segs {
		joined.WriteString(seg.Text)
	}
	if strings.Contains(joined.String(), "tab") {
		t.Fatalf("expected compact legend to avoid tab hint: %q", joined.String())
	}
	if !strings.Contains(joined.String(), "enter/spc") {
		t.Fatalf("expected compact legend to include enter/spc: %q", joined.String())
	}
}

func TestLegendForBrowseStaysLean(t *testing.T) {
	segs := legendForStatus(&StatusBarWidget{Mode: StatusBrowse, DaemonOnline: true})
	var joined strings.Builder
	for _, seg := range segs {
		joined.WriteString(seg.Text)
	}
	if !strings.Contains(joined.String(), "j/k move") {
		t.Fatalf("expected browse legend to keep movement hint: %q", joined.String())
	}
	if !strings.Contains(joined.String(), "/ filter") {
		t.Fatalf("expected browse legend to keep filter hint: %q", joined.String())
	}
	if strings.Contains(joined.String(), "enter/O") {
		t.Fatalf("expected browse legend to omit enter option hint: %q", joined.String())
	}
	if strings.Contains(joined.String(), "space select detail") {
		t.Fatalf("expected browse legend to omit space detail hint: %q", joined.String())
	}
	if strings.Contains(joined.String(), "g/G top/bot") {
		t.Fatalf("expected browse legend to omit top/bot hint: %q", joined.String())
	}
}

func TestOptionsModalLegendUsesContextSpecificActions(t *testing.T) {
	modal := NewOptionsModal("demo", []OptionsModalEntry{{Label: "Resume", Action: func() {}}})
	actions := modal.StatusLegendActions()
	if len(actions) != 2 || actions[0] != LegendMove || actions[1] != LegendClose {
		t.Fatalf("unexpected session-context legend actions: %#v", actions)
	}

	modal.Context = OptionsModalContextReturn
	actions = modal.StatusLegendActions()
	if len(actions) != 3 || actions[2] != LegendQuit {
		t.Fatalf("expected return-context legend to include quit, got %#v", actions)
	}
}

func TestFormatHeartbeatAgeUsesSeconds(t *testing.T) {
	if got := formatHeartbeatAge(24*time.Second + 200*time.Millisecond); got != "24s" {
		t.Fatalf("heartbeat age = %q, want 24s", got)
	}
	if got := formatHeartbeatAge(-time.Second); got != "0s" {
		t.Fatalf("negative heartbeat age = %q, want 0s", got)
	}
}

func TestLegendActionAtFindsRefreshHint(t *testing.T) {
	bar := &StatusBarWidget{Mode: StatusBrowse, DaemonOnline: true}
	r := Rect{X: 0, Y: 0, W: 120, H: 1}
	found := false
	for x := range r.W {
		action, ok := legendActionAt(bar, r, x)
		if ok && action == LegendRefresh {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected to find refresh legend hit target")
	}
}

func TestBadgeTextColorTracksLightPalette(t *testing.T) {
	defer applyTUITheme(detectedTerminalTheme)

	applyTUITheme(terminalThemeLight)
	if got := badgeTextColor(ColorAccent); got != tcell.ColorWhite {
		t.Fatalf("light accent badge text = %v, want white", got)
	}
	if got := badgeTextColor(ColorWarning); got != tcell.ColorBlack {
		t.Fatalf("light warning badge text = %v, want black", got)
	}

	applyTUITheme(terminalThemeDark)
	if got := badgeTextColor(ColorAccent); got != tcell.ColorBlack {
		t.Fatalf("dark accent badge text = %v, want black", got)
	}
}

func TestAccountStatusSegmentQuietWhenHealthy(t *testing.T) {
	_, _, ok := accountStatusSegment(&StatusBarWidget{})
	if ok {
		t.Fatal("expected no account segment when all accounts are ready")
	}
}

func TestAccountStatusSegmentWorstSeverityFirst(t *testing.T) {
	txt, style, ok := accountStatusSegment(&StatusBarWidget{AccountNeedsReauthCount: 2, AccountThrottledCount: 3})
	if !ok {
		t.Fatal("expected a segment when accounts are degraded")
	}
	if !strings.Contains(txt, "needs re-auth") || !strings.Contains(txt, "2") {
		t.Fatalf("expected needs-re-auth count first, got %q", txt)
	}
	if style != badgeStyle(ColorError) {
		t.Fatalf("expected ColorError style for needs-re-auth, got %v", style)
	}
}

func TestAccountStatusSegmentThrottledOnly(t *testing.T) {
	txt, style, ok := accountStatusSegment(&StatusBarWidget{AccountThrottledCount: 4})
	if !ok {
		t.Fatal("expected a segment when accounts are throttled")
	}
	if !strings.Contains(txt, "throttled") || !strings.Contains(txt, "4") {
		t.Fatalf("expected throttled count, got %q", txt)
	}
	if style != badgeStyle(ColorWarning) {
		t.Fatalf("expected ColorWarning style for throttled, got %v", style)
	}
}
