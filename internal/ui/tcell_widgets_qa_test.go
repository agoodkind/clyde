package ui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"goodkind.io/clyde/internal/session"
)

// TestQA_AllWidgets renders every standalone widget the TUI exposes
// against a tcell.SimulationScreen, dumps the cell buffer to ASCII,
// and writes one file per widget+state under qa-frames/. The result
// is a flip-book a human reviewer can scan in minutes to spot
// rendering regressions across the whole TUI.
//
// Set CLYDE_QA_DIR to override the output directory; defaults to
// the OS temp dir. Each test sub-case is independent so a single
// regression does not abort the whole sweep.
func TestQA_AllWidgets(t *testing.T) {
	dir := os.Getenv("CLYDE_QA_DIR")
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	type widgetCase struct {
		name string
		w, h int
		draw func(scr tcell.Screen, r Rect)
	}

	cases := []widgetCase{
		{"00-tabbar-sessions-active", 100, 1, func(scr tcell.Screen, r Rect) {
			tb := NewTabBar([]string{"Sessions", "Stats", "Settings", "Sidecar"})
			tb.Active = 0
			tb.Draw(scr, r)
		}},
		{"01-tabbar-settings-active", 100, 1, func(scr tcell.Screen, r Rect) {
			tb := NewTabBar([]string{"Sessions", "Stats", "Settings", "Sidecar"})
			tb.Active = 2
			tb.Draw(scr, r)
		}},

		{"02-statusbar-browse", 100, 1, func(scr tcell.Screen, r Rect) {
			s := &StatusBarWidget{Mode: StatusBrowse, Position: "Top"}
			s.Draw(scr, r)
		}},
		{"03-statusbar-detail-with-live-urls", 100, 1, func(scr tcell.Screen, r Rect) {
			s := &StatusBarWidget{Mode: StatusDetail, Position: "45%", LiveURLCount: 3}
			s.Draw(scr, r)
		}},
		{"04-statusbar-compact", 100, 1, func(scr tcell.Screen, r Rect) {
			s := &StatusBarWidget{Mode: StatusCompact}
			s.Draw(scr, r)
		}},

		{"05-table-empty", 100, 10, func(scr tcell.Screen, r Rect) {
			tw := NewTableWidget([]string{"NAME", "DIR", "MODEL", "MSGS", "LAST ACTIVE"})
			tw.Draw(scr, r)
		}},
		{"06-table-populated-no-selection", 100, 12, func(scr tcell.Screen, r Rect) {
			tw := mkTable(false)
			tw.Draw(scr, r)
		}},
		{"07-table-populated-row3-selected", 100, 12, func(scr tcell.Screen, r Rect) {
			tw := mkTable(true)
			tw.SelectedRow = 2
			tw.Draw(scr, r)
		}},
		{"08-table-sort-by-name-asc", 100, 12, func(scr tcell.Screen, r Rect) {
			tw := mkTable(false)
			tw.SortCol = 0
			tw.SortAsc = true
			tw.Draw(scr, r)
		}},

		{"09-optionsmodal-session-actions", 100, 25, func(scr tcell.Screen, r Rect) {
			m := NewOptionsModal("clotilde-tcell-remastered", []OptionsModalEntry{
				{Label: "Resume", Hint: "load this session"},
				{Label: "View transcript", Hint: "v"},
				{Label: "Compact", Hint: "c"},
				{Label: "Search inside", Hint: "s"},
				{Label: "Fork", Hint: "f"},
				{Label: "Rename", Hint: ""},
				{Label: "Edit basedir", Hint: "B"},
				{Label: "Drive in sidecar", Hint: "live stream", Disabled: true},
				{Label: "Open live URL", Hint: "browser", Disabled: true},
				{Label: "Copy live URL", Hint: "clipboard", Disabled: true},
				{Label: "Delete", Hint: "d"},
			})
			m.Draw(scr, r)
		}},

		{"10-returnprompt", 100, 25, func(scr tcell.Screen, r Rect) {
			m := NewOptionsModal("Session exited: clotilde-tcell-remastered", []OptionsModalEntry{
				{Label: "Quit clyde", Hint: "q"},
				{Label: "Return back to chat", Hint: "enter"},
				{Label: "Resume", Hint: "load this session"},
				{Label: "View transcript", Hint: "v"},
				{Label: "Edit basedir", Hint: "b"},
				{Label: "Drive in sidecar", Hint: "live stream", Disabled: true},
				{Label: "Open live URL", Hint: "browser", Disabled: true},
				{Label: "Copy live URL", Hint: "clipboard", Disabled: true},
				{Label: "Rename", Hint: "edits the visible name"},
				{Label: "Compact", Hint: "c"},
				{Label: "Fork", Hint: "f"},
				{Label: "Delete", Hint: "d"},
			})
			m.Context = OptionsModalContextReturn
			m.StatsSegments = [][]TextSegment{
				{{Text: "Identity", Style: StyleDefault.Bold(true)}},
				{{Text: "  Model", Style: StyleSubtext}, {Text: "opus 1M", Style: StyleDefault}},
				{{Text: "  Basedir", Style: StyleSubtext}, {Text: "~/Sites/clotilde", Style: StyleDefault}},
				{{Text: "Timing", Style: StyleDefault.Bold(true)}},
				{{Text: "  Last used", Style: StyleSubtext}, {Text: "7 minutes ago", Style: StyleDefault}},
			}
			m.resetCursor()
			m.Draw(scr, r)
		}},

		{"15-filepicker", 100, 25, func(scr tcell.Screen, r Rect) {
			home, _ := os.UserHomeDir()
			fp := NewFilePickerOverlay("Pick basedir for new session", home)
			fp.Draw(scr, r)
		}},

		{"16-modal-confirm-delete", 100, 12, func(scr tcell.Screen, r Rect) {
			m := &Modal{
				Title:       "Delete session",
				Body:        "Permanently remove clotilde-tcell-remastered and its transcript?",
				Details:     []string{"1,723 chain entries", "4.96 MB on disk", "3 prior compactions"},
				Buttons:     []string{"Cancel", "Delete"},
				ActiveIndex: 0,
				Destructive: true,
			}
			m.Draw(scr, r)
		}},

		{"17-details-wide", 200, 30, func(scr tcell.Screen, r Rect) {
			d := mkDetailsView()
			d.Draw(scr, r)
		}},
		{"18-details-stacked-narrow", 70, 30, func(scr tcell.Screen, r Rect) {
			d := mkDetailsView()
			d.Draw(scr, r)
		}},
		{"19-details-tiny", 45, 25, func(scr tcell.Screen, r Rect) {
			d := mkDetailsView()
			d.Draw(scr, r)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scr := tcell.NewSimulationScreen("UTF-8")
			if err := scr.Init(); err != nil {
				t.Fatal(err)
			}
			defer scr.Fini()
			scr.SetSize(tc.w, tc.h)
			for y := range tc.h {
				for x := range tc.w {
					scr.SetContent(x, y, ' ', nil, tcell.StyleDefault)
				}
			}
			tc.draw(scr, Rect{X: 0, Y: 0, W: tc.w, H: tc.h})
			scr.Show()

			cells, cw, _ := scr.GetContents()
			var b strings.Builder
			fmt.Fprintf(&b, "# qa/%s.txt  size=%dx%d\n\n", tc.name, tc.w, tc.h)
			for y := range tc.h {
				row := make([]rune, 0, tc.w)
				for x := range tc.w {
					c := cells[y*cw+x]
					if len(c.Runes) == 0 || c.Runes[0] == 0 {
						row = append(row, ' ')
						continue
					}
					row = append(row, c.Runes[0])
				}
				fmt.Fprintln(&b, strings.TrimRight(string(row), " "))
			}
			path := dir + "/qa-" + tc.name + ".txt"
			if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			t.Logf("wrote %s", path)
		})
	}
}

// mkTable builds a populated TableWidget mirroring the dashboard's
// Sessions table. activeOnly=true marks the table as user-interacted
// so the SelectedRow highlight renders.
func mkTable(activeOnly bool) *TableWidget {
	tw := NewTableWidget([]string{"NAME", "DIR", "LAST ACTIVE", "MODEL", "MSGS", "SUMMARY", "CREATED"})
	tw.Active = activeOnly
	rows := [][]string{
		{"clotilde-tcell-remastered", "~/Sites/clotilde", "now", "opus", "1.7k", "context tokenizer parity", "1 day ago"},
		{"agoodkind-f559acdd", "~", "2h ago", "opus", "1.7k", "Emdash hook refinement", "3 days ago"},
		{"clotilde-b2a47cef", "~/Sites/clotilde", "6h ago", "opus", "402", "compaction pipeline", "1 day ago"},
		{"swift-integration-tests-fix", "~/Sites/macos-smc-fan", "14h ago", "opus", "812", "fan curve calibration", "4 days ago"},
		{"motd-shell-rules-cleanup", "~/.dotfiles", "3h ago", "sonnet", "1.2k", "shell ergonomics", "2026-04-09"},
	}
	for _, r := range rows {
		cells := make([]TableCell, len(r))
		for i, s := range r {
			cells[i] = TableCell{Text: s}
		}
		tw.Rows = append(tw.Rows, cells)
	}
	return tw
}

// mkDetailsView builds a populated DetailsView with stats + messages
// so the wide, stacked, and tiny layouts all have something to render.
func mkDetailsView() *DetailsView {
	d := NewDetailsView()
	sess := &session.Session{
		Name: "clotilde-tcell-remastered",
		Metadata: session.Metadata{
			Name:          "clotilde-tcell-remastered",
			SessionID:     "f9d61101-6e3b-43b4-b3e4-7cd70e5fa228",
			WorkspaceRoot: "/Users/me/Sites/clotilde",
			Created:       time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC),
			LastAccessed:  time.Now().Add(-2 * time.Hour),
			Context:       "Clotilde context tokenizer parity",
		},
	}
	detail := SessionDetail{
		Model: "opus",
		AllMessages: []DetailMessage{
			{Role: "user", Text: "OK exiting and returning see u in 30s", Timestamp: time.Now().Add(-3 * time.Hour)},
			{Role: "assistant", Text: "see you in 30s, backup at PRE-300K-PURECONV...", Timestamp: time.Now().Add(-3 * time.Hour)},
			{Role: "user", Text: "back", Timestamp: time.Now().Add(-2 * time.Hour)},
			{Role: "assistant", Text: "Back. Resumed clotilde-tcell-remastered, continuing where we left off.", Timestamp: time.Now().Add(-2 * time.Hour)},
			{Role: "user", Text: "what's next?", Timestamp: time.Now().Add(-1 * time.Hour)},
		},
	}
	d.Set(sess, detail)
	d.Focus = DetailsFocusRight
	return d
}
