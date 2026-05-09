package ui

import (
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestMarkedSliderRendersHistoryStates(t *testing.T) {
	marks := []string{"C1", "C2", "C3", "VISIBLE"}
	tests := []struct {
		name     string
		selected int
		want     string
	}{
		{name: "full", selected: 0, want: "[[C1]========C2========C3========VISIBLE]"},
		{name: "middle", selected: 1, want: "[C1........[C2]========C3========VISIBLE]"},
		{name: "default latest", selected: 2, want: "[C1........C2........[C3]========VISIBLE]"},
		{name: "visible only", selected: 3, want: "[C1........C2........C3........[VISIBLE]]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (MarkedSlider{Marks: marks, Selected: tt.selected}).Render()
			if got != tt.want {
				t.Fatalf("slider = %q want %q", got, tt.want)
			}
		})
	}
}

func TestExportPanelDefaultsToLatestCompactionPlusVisible(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{Compactions: 3, VisibleMessages: 10, VisibleTokensEstimate: 4000}, "/tmp")
	if panel.historyStart != 2 {
		t.Fatalf("historyStart = %d want 2", panel.historyStart)
	}
	if got := panel.historyMarkedSlider().Render(); got != "[C1........C2........[C3]========VISIBLE]" {
		t.Fatalf("slider = %q", got)
	}
}

func TestExportPanelDefaultsToVisibleOnlyWithoutCompactions(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{}, "/tmp")
	if panel.historyStart != 0 {
		t.Fatalf("historyStart = %d want 0", panel.historyStart)
	}
	if got := panel.historyMarkedSlider().Render(); got != "[[VISIBLE]]" {
		t.Fatalf("slider = %q", got)
	}
}

func TestExportPanelHistorySliderKeepsSelectedMarkVisibleWhenDense(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{Compactions: 14}, "/tmp")
	got := panel.historySliderForWidth(54)
	if runeCount(got) > 54 {
		t.Fatalf("compact slider width = %d, want <= 54: %q", runeCount(got), got)
	}
	for _, want := range []string{"C1", "[C14]", "VISIBLE"} {
		if !strings.Contains(got, want) {
			t.Fatalf("compact slider missing %q: %q", want, got)
		}
	}
}

func TestExportPanelEstimateDoesNotDuplicateTokenUnit(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{
		Compactions:           1,
		VisibleMessages:       10,
		VisibleTokensEstimate: 1200,
		ToolCalls:             2,
		SystemPrompts:         1,
		ToolResultMessages:    1,
		AssistantMessages:     1,
		UserMessages:          1,
		TranscriptSizeBytes:   1024,
	}, "/tmp")
	got := panel.estimateLabel()
	if strings.Contains(got, "tok tokens") {
		t.Fatalf("estimate duplicated token unit: %q", got)
	}
	if !strings.Contains(got, "messages") || !strings.Contains(got, "compaction snapshot") {
		t.Fatalf("estimate missing expected fields: %q", got)
	}
}

func TestExportPanelBuildsRequestFromState(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{Compactions: 2}, "/tmp/out")
	panel.historyStart = 0
	panel.includeSystemPrompts = true
	panel.includeToolOutputs = true
	panel.includeRawJSONMetadata = true
	panel.whitespace = SessionExportWhitespaceDense
	panel.copyToClipboard = false
	panel.saveToFile = true
	panel.overwrite = true
	panel.name = "demo-export.md"

	req := panel.buildRequest()
	if req.SessionName != "demo" || req.HistoryStart != 0 {
		t.Fatalf("unexpected request identity: %+v", req)
	}
	if !req.IncludeSystemPrompts || !req.IncludeToolOutputs || !req.IncludeRawJSONMetadata {
		t.Fatalf("content toggles missing from request: %+v", req)
	}
	if req.WhitespaceCompression != SessionExportWhitespaceDense {
		t.Fatalf("whitespace compression = %q want dense", req.WhitespaceCompression)
	}
	if req.CopyToClipboard || !req.SaveToFile || !req.Overwrite {
		t.Fatalf("destination toggles wrong: %+v", req)
	}
	if req.Directory != "/tmp/out" || req.Filename != "demo-export.md" {
		t.Fatalf("destination wrong: %+v", req)
	}
}

func TestDefaultExportFilenameAutoDatesAndSanitizes(t *testing.T) {
	got := defaultExportFilename("My Feature/Branch", SessionExportMarkdown)
	if !strings.HasSuffix(got, "-my-feature-branch.md") {
		t.Fatalf("filename = %q, want dated sanitized markdown name", got)
	}
	if len(got) < len("2006-01-02-") || got[4] != '-' || got[7] != '-' {
		t.Fatalf("filename = %q, want yyyy-mm-dd prefix", got)
	}
}

func TestExportPanelFolderActionOpensPicker(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{}, "/tmp")
	called := false
	panel.OnChooseFolder = func(*ExportPanel) { called = true }
	panel.focusGroup = 5

	if !panel.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)) {
		t.Fatalf("expected folder enter to be handled")
	}
	if !called {
		t.Fatalf("expected folder picker callback")
	}
}

func TestExportPanelDrawsStatsAndSlider(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{
		VisibleTokensEstimate: 1200,
		VisibleMessages:       12,
		UserMessages:          6,
		AssistantMessages:     6,
		ToolResultMessages:    3,
		ToolCalls:             4,
		SystemPrompts:         2,
		Compactions:           1,
		TranscriptSizeBytes:   4096,
	}, "/tmp")
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("screen init: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(100, 34)
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 100, H: 34})
	scr.Show()
	text := compactPanelScreenText(scr)
	for _, want := range []string{"visible tokens", "tool calls", "system prompts", "whitespace", "Trim leading/trailing whitespace", "[[C1]========VISIBLE]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("draw missing %q:\n%s", want, text)
		}
	}
}

func TestExportPanelSmallViewportScrollsWithoutOverlappingActions(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{
		VisibleTokensEstimate: 402000,
		VisibleMessages:       756,
		UserMessages:          206,
		AssistantMessages:     2908,
		ToolResultMessages:    2358,
		ToolCalls:             2358,
		SystemPrompts:         297,
		Compactions:           3,
		TranscriptSizeBytes:   12 * 1024 * 1024,
	}, "/tmp")
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("screen init: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(78, 24)
	panel.Draw(scr, Rect{X: 0, Y: 0, W: 78, H: 24})
	scr.Show()

	text := compactPanelScreenText(scr)
	if strings.Contains(text, "Actionspace") {
		t.Fatalf("actions overlapped compression row:\n%s", text)
	}
	if panel.contentHeight <= panel.contentRect.H {
		t.Fatalf("test expected overflowing content, height=%d viewport=%d", panel.contentHeight, panel.contentRect.H)
	}

	x := panel.contentRect.X + 1
	y := panel.contentRect.Y + 1
	if !panel.HandleEvent(tcell.NewEventMouse(x, y, tcell.WheelDown, tcell.ModNone)) {
		t.Fatalf("expected mouse wheel to be handled")
	}
	if panel.scrollOffset == 0 {
		t.Fatalf("expected mouse wheel to advance scroll offset")
	}

	if !panel.HandleEvent(tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone)) {
		t.Fatalf("expected page up to be handled")
	}
	if panel.scrollOffset != 0 {
		t.Fatalf("page up should return to top, offset=%d", panel.scrollOffset)
	}
}

func TestExportWhitespaceDescriptionsUseFullCopy(t *testing.T) {
	tests := []struct {
		mode SessionExportWhitespaceCompression
		want string
	}{
		{SessionExportWhitespacePreserve, "Keep rendered export spacing as-is."},
		{SessionExportWhitespaceTidy, "Trim leading/trailing whitespace, collapse extra spaces in prose, and reduce multiple blank lines to one blank line."},
		{SessionExportWhitespaceCompact, "Apply tidy cleanup, then make the transcript more paste-friendly"},
		{SessionExportWhitespaceDense, "Remove blank lines where possible for the smallest readable export"},
	}
	for _, tt := range tests {
		if got := exportWhitespaceDescription(tt.mode); !strings.Contains(got, tt.want) {
			t.Fatalf("description for %q = %q, want containing %q", tt.mode, got, tt.want)
		}
	}
}

func TestExportPanelCyclesWhitespaceCompression(t *testing.T) {
	panel := newExportPanelWithTitle("demo", "demo", SessionExportStats{}, "/tmp")
	panel.focusGroup = 3
	if panel.whitespace != SessionExportWhitespaceTidy {
		t.Fatalf("default whitespace = %q want tidy", panel.whitespace)
	}
	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone))
	if panel.whitespace != SessionExportWhitespaceCompact {
		t.Fatalf("whitespace = %q want compact", panel.whitespace)
	}
	_ = panel.HandleEvent(tcell.NewEventKey(tcell.KeyRight, 0, tcell.ModNone))
	if panel.whitespace != SessionExportWhitespaceDense {
		t.Fatalf("whitespace = %q want dense", panel.whitespace)
	}
}
