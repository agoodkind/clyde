package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestOptionsModalSpaceAndEnterBothActivate(t *testing.T) {
	for _, key := range []tcell.Key{tcell.KeyEnter, tcell.KeyRune} {
		t.Run(fmt.Sprintf("key-%v", key), func(t *testing.T) {
			activated := 0
			modal := NewOptionsModal("x", []OptionsModalEntry{
				{
					Label: "Resume",
					Action: func() {
						activated++
					},
				},
			})
			var ev *tcell.EventKey
			if key == tcell.KeyEnter {
				ev = tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone)
			} else {
				ev = tcell.NewEventKey(tcell.KeyRune, ' ', tcell.ModNone)
			}
			handled := modal.HandleEvent(ev)
			if !handled {
				t.Fatalf("key %v was not handled", key)
			}
			if activated != 1 {
				t.Fatalf("key %v activated=%d want 1", key, activated)
			}
		})
	}
}

func TestOptionsModalIncludesGapBetweenTopAndSharedRows(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(90, 24)

	modal := NewOptionsModal("return", []OptionsModalEntry{
		{Label: "Resume", Action: func() {}},
	})
	modal.Context = OptionsModalContextReturn
	modal.TopEntries = []OptionsModalEntry{
		{Label: "Quit clyde", Action: func() {}},
	}
	modal.Draw(scr, Rect{X: 0, Y: 0, W: 90, H: 24})

	if modal.optionsTotalRows != 3 {
		t.Fatalf("optionsTotalRows=%d want 3 (top + gap + base)", modal.optionsTotalRows)
	}
}

func TestOptionsModalOutsideClickDoesNotCancel(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(80, 24)

	cancelled := 0
	modal := NewOptionsModal("session", []OptionsModalEntry{
		{Label: "Resume", Action: func() {}},
	})
	modal.OnCancel = func() {
		cancelled++
	}
	modal.Draw(scr, Rect{X: 0, Y: 0, W: 80, H: 24})

	if modal.rect.Contains(0, 0) {
		t.Fatalf("test click unexpectedly inside modal rect %#v", modal.rect)
	}
	if !modal.HandleEvent(tcell.NewEventMouse(0, 0, tcell.Button1, tcell.ModNone)) {
		t.Fatalf("outside click was not handled")
	}
	if cancelled != 0 {
		t.Fatalf("outside click cancelled modal %d times, want 0", cancelled)
	}
}

func TestOptionsModalMouseWheelScrollsStatsPane(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(100, 20)

	segments := make([][]TextSegment, 0, 40)
	for i := range 40 {
		segments = append(segments, []TextSegment{{Text: fmt.Sprintf("line %d", i), Style: StyleDefault}})
	}

	entries := make([]OptionsModalEntry, 0, 20)
	for i := range 20 {
		entries = append(entries, OptionsModalEntry{
			Label:  fmt.Sprintf("Entry %d", i),
			Action: func() {},
		})
	}
	modal := NewOptionsModal("stats", entries)
	modal.StatsSegments = segments
	modal.Draw(scr, Rect{X: 0, Y: 0, W: 100, H: 20})

	if modal.statsRect.W == 0 || modal.statsRect.H == 0 {
		t.Fatalf("stats pane not drawn")
	}
	x := modal.statsRect.X + 1
	y := modal.statsRect.Y + 1
	handled := modal.HandleEvent(tcell.NewEventMouse(x, y, tcell.WheelDown, tcell.ModNone))
	if !handled {
		t.Fatalf("wheel event over stats pane not handled")
	}
	if modal.statsBox == nil || modal.statsBox.Offset <= 0 {
		t.Fatalf("stats offset not advanced after wheel scroll")
	}
}

func TestOptionsModalOptionsScrollbarDragScrolls(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(70, 12)

	entries := make([]OptionsModalEntry, 0, 30)
	for i := range 30 {
		entries = append(entries, OptionsModalEntry{
			Label:  fmt.Sprintf("Entry %d", i),
			Action: func() {},
		})
	}
	modal := NewOptionsModal("many", entries)
	modal.Draw(scr, Rect{X: 0, Y: 0, W: 70, H: 12})
	if modal.optionsScrollbarRect.H == 0 {
		t.Fatalf("expected options scrollbar for long list")
	}

	dragX := modal.optionsScrollbarRect.X
	dragY := modal.optionsScrollbarRect.Y + modal.optionsScrollbarRect.H - 1
	_ = modal.HandleEvent(tcell.NewEventMouse(dragX, dragY, tcell.Button1, tcell.ModNone))
	if modal.optionsOffset == 0 {
		t.Fatalf("expected options offset to increase after scrollbar drag start")
	}
}

func TestOptionsModalKeyboardNavigationScrollsOptionsPane(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(70, 12)

	entries := make([]OptionsModalEntry, 0, 30)
	for i := range 30 {
		entries = append(entries, OptionsModalEntry{
			Label:  fmt.Sprintf("Entry %d", i),
			Action: func() {},
		})
	}
	modal := NewOptionsModal("many", entries)
	modal.Draw(scr, Rect{X: 0, Y: 0, W: 70, H: 12})

	for i := range 15 {
		if !modal.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)) {
			t.Fatalf("down key %d not handled", i)
		}
	}

	if modal.optionsOffset == 0 {
		t.Fatalf("optionsOffset did not advance after keyboard navigation")
	}
	logical, ok := modal.logicalRowForEntryIndex(modal.cursor)
	if !ok {
		t.Fatalf("cursor has no logical row")
	}
	if logical < modal.optionsOffset || logical >= modal.optionsOffset+modal.optionsVisibleRows {
		t.Fatalf("cursor logical row %d outside visible window [%d,%d)", logical, modal.optionsOffset, modal.optionsOffset+modal.optionsVisibleRows)
	}
}

func TestOptionsModalHintKeepsBreathingRoomFromLabel(t *testing.T) {
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatalf("init simulation screen: %v", err)
	}
	defer scr.Fini()
	scr.SetSize(100, 25)

	modal := NewOptionsModal("return", []OptionsModalEntry{
		{Label: "Quit clyde", Hint: "q", Action: func() {}},
		{Label: "Return back to chat", Hint: "enter", Action: func() {}},
		{Label: "Resume", Hint: "load this session", Action: func() {}},
	})
	modal.Context = OptionsModalContextReturn
	modal.Draw(scr, Rect{X: 0, Y: 0, W: 100, H: 25})
	scr.Show()

	cells, width, _ := scr.GetContents()
	rowY := modal.entryRects[1].Y
	row := make([]rune, 0, width)
	for x := range width {
		cell := cells[rowY*width+x]
		if len(cell.Runes) == 0 || cell.Runes[0] == 0 {
			row = append(row, ' ')
			continue
		}
		row = append(row, cell.Runes[0])
	}
	line := string(row)
	labelIdx := strings.Index(line, "Return back to chat")
	hintIdx := strings.Index(line, "enter")
	if labelIdx < 0 || hintIdx < 0 {
		t.Fatalf("row missing label or hint: %q", line)
	}
	gap := hintIdx - (labelIdx + len("Return back to chat"))
	if gap < optionsModalHintMinGap {
		t.Fatalf("gap=%d want >= %d in row %q", gap, optionsModalHintMinGap, line)
	}
}
