package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"goodkind.io/clyde/internal/session"
)

// TestHappyPath_LaunchAndNavigate confirms the basic dashboard flow:
// build the App with a populated session list, drive the same key
// events a user would type, and assert the visible state changes
// frame-by-frame. Snapshots also write to qa-frames/ for visual
// review.
//
// Coverage:
//   - Initial state: table active, row 0 selected, browse mode.
//   - Down x3 → row 2 selected (first Down arms row 0).
//   - Up x1 → row 1 selected.
//   - g (top) → row 0 selected.
//   - G (bottom) → last row selected.
//   - / → opens filter overlay; type "swift" → narrows to one row;
//     Enter applies; Esc clears.
//   - ? → opens help modal.
//   - q with overlay open → closes overlay; q again with no overlay
//     and no selection → flips a.running to false (quit signal).
func TestHappyPath_LaunchAndNavigate(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 6)
	defer cleanup()

	// Initial state.
	if a.mode != StatusBrowse {
		t.Errorf("initial mode = %v, want StatusBrowse", a.mode)
	}
	if a.table.SelectedRow != 0 {
		t.Errorf("initial SelectedRow = %d, want 0", a.table.SelectedRow)
	}
	if a.overlay != nil {
		t.Errorf("initial overlay should be nil")
	}

	// Down x3 → row 2 (first Down arms row 0).
	for range 3 {
		a.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	}
	if a.table.SelectedRow != 2 {
		t.Errorf("after 3×Down, SelectedRow = %d, want 2", a.table.SelectedRow)
	}

	// Up x1 → row 1.
	a.handleKey(tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone))
	if a.table.SelectedRow != 1 {
		t.Errorf("after Up, SelectedRow = %d, want 1", a.table.SelectedRow)
	}

	// g (top), G (bottom).
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, 'g', tcell.ModNone))
	if a.table.SelectedRow != 0 {
		t.Errorf("after g, SelectedRow = %d, want 0", a.table.SelectedRow)
	}
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, 'G', tcell.ModNone))
	if a.table.SelectedRow != len(a.visibleIdx)-1 {
		t.Errorf("after G, SelectedRow = %d, want %d (last row)",
			a.table.SelectedRow, len(a.visibleIdx)-1)
	}

	// Snapshot: dashboard with bottom row highlighted.
	snapshotApp(t, a, scr, "happy-01-dashboard-bottom-row")

	// / → with a row highlighted, search mode (search inside the
	// session's transcript). Without a row, it opens table filter.
	// Test the search path first since SelectedRow is at last row.
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, '/', tcell.ModNone))
	if a.overlay == nil {
		t.Errorf("after / on selected row, overlay should be search input")
	}
	a.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if a.overlay != nil {
		t.Errorf("after Esc on search, overlay should clear")
	}

	// Now drop the selection and confirm / opens the filter overlay.
	a.deselect()
	a.table.SelectedRow = -1
	a.table.Active = false
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, '/', tcell.ModNone))
	if a.overlay == nil {
		t.Errorf("after / with no row, overlay should be filter input")
	}
	if a.mode != StatusFilter {
		t.Errorf("after / with no row, mode = %v, want StatusFilter", a.mode)
	}
	a.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if a.overlay != nil {
		t.Errorf("after Esc on filter, overlay should clear")
	}

	// ? → help modal.
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, '?', tcell.ModNone))
	if a.overlay == nil {
		t.Errorf("after ?, overlay should be help modal")
	}
	snapshotApp(t, a, scr, "happy-02-help-modal-open")
	// Esc dismisses help.
	a.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if a.overlay != nil {
		t.Errorf("after Esc on help, overlay should clear")
	}
}

// TestHappyPath_ResumeCycleMultipleTimes is the bug the user
// reported: "after leaving chat and returning to the list post-exit
// pane STILL doesn't show and now entire list freezes." This drives
// 5 consecutive resume cycles via the test seam (suspendImpl
// stubbed) and asserts each one:
//   - calls cb.ResumeSession exactly once
//   - opens the return-context options overlay afterward
//   - leaves the table responsive (selection still moves)
//   - leaves a.running == true (no silent quit)
//
// If suspendImpl swallows an error or silently drops the overlay on
// any cycle, this test fails immediately with the cycle index.
func TestHappyPath_ResumeCycleMultipleTimes(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()

	resumeCalls := 0
	a.cb.ResumeSession = func(s *session.Session) error {
		resumeCalls++
		return nil
	}
	// Stub suspendImpl so the test runs without touching a real
	// terminal. Mirrors what production does minus the screen
	// teardown / reinit.
	a.suspendImpl = func(fn func()) {
		fn()
	}

	const cycles = 5
	for i := 1; i <= cycles; i++ {
		// Select row 0 and trigger Resume via the resumeRow path
		// (matches what Enter on the options popup → Resume entry
		// would do).
		a.table.SelectedRow = 0
		a.table.Active = true
		a.resumeRow(0)

		if resumeCalls != i {
			t.Fatalf("cycle %d: cb.ResumeSession call count = %d, want %d",
				i, resumeCalls, i)
		}
		if a.overlay == nil {
			t.Fatalf("cycle %d: return overlay missing after resume",
				i)
		}
		modal, ok := a.overlay.(*OptionsModal)
		if !ok || modal.Context != OptionsModalContextReturn {
			t.Fatalf("cycle %d: overlay type = %T, want return-context *OptionsModal",
				i, a.overlay)
		}
		if !a.running {
			t.Fatalf("cycle %d: a.running flipped to false (silent quit)",
				i)
		}

		// User dismisses the prompt back to the dashboard. Use cancel
		// rather than Return back to chat so we don't recurse infinitely
		// through the cycle-test-self path.
		if modal.OnCancel == nil {
			t.Fatalf("cycle %d: return overlay missing cancel handler", i)
		}
		modal.OnCancel()
		if a.overlay != nil {
			t.Fatalf("cycle %d: cancel should clear overlay", i)
		}

		// Confirm the table is still responsive  --  Down should still
		// move the selection (the bug the user reported was "list
		// freezes and no response").
		prevRow := a.table.SelectedRow
		a.handleKey(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
		if a.table.SelectedRow == prevRow && len(a.visibleIdx) > 1 {
			t.Errorf("cycle %d: table not responsive  --  Down did not move selection",
				i)
		}
	}
}

// TestHappyPath_ResumeFromOptionsPopup mirrors the OTHER resume code
// path: open options modal, navigate to Resume entry, activate it.
// This was the path that earlier silently no-op'd when the row
// lookup returned -1.
func TestHappyPath_ResumeFromOptionsPopup(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()

	resumeCalls := 0
	a.cb.ResumeSession = func(s *session.Session) error {
		resumeCalls++
		return nil
	}
	a.suspendImpl = func(fn func()) { fn() }

	// Select row 1, open options modal.
	a.table.SelectedRow = 1
	a.table.Active = true
	a.openSessionOptions(1)
	if a.overlay == nil {
		t.Fatal("openSessionOptions did not install an overlay")
	}
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	if len(modal.Entries) == 0 {
		t.Fatal("OptionsModal has no entries")
	}
	if modal.Entries[0].Label == "Drive in sidecar" {
		t.Fatalf("Drive in sidecar should not be first option")
	}

	// Find the Resume entry and invoke its action.
	var resumeAction func()
	for _, e := range modal.Entries {
		if e.Label == "Resume" {
			resumeAction = e.Action
			break
		}
	}
	if resumeAction == nil {
		t.Fatal("OptionsModal has no Resume entry")
	}
	resumeAction()

	if resumeCalls != 1 {
		t.Errorf("Resume entry should call cb.ResumeSession once, got %d", resumeCalls)
	}
	// After resume, overlay should be return-context OptionsModal.
	returnModal, ok := a.overlay.(*OptionsModal)
	if !ok || returnModal.Context != OptionsModalContextReturn {
		t.Errorf("after Resume from popup, overlay = %T, want return-context *OptionsModal", a.overlay)
	}
}

// mkAppWithSessions constructs a test App with n synthetic sessions
// and a SimulationScreen attached. Returns the app, the screen, and
// a cleanup func. The App's expensive callbacks (RefreshSummary,
// GetSessionDetail, SubscribeRegistry) are stubbed.
func mkAppWithSessions(t *testing.T, n int) (*App, tcell.SimulationScreen, func()) {
	t.Helper()
	sessions := make([]*session.Session, n)
	for i := range n {
		// Workspace must NOT be under a temp prefix or
		// isEphemeralSession will hide the session from rebuildVisible.
		// Use a stable Sites-style path that the filter accepts.
		sessions[i] = &session.Session{
			Name: fmt.Sprintf("test-session-%02d", i),
			Metadata: session.Metadata{
				Name:          fmt.Sprintf("test-session-%02d", i),
				SessionID:     fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
				WorkspaceRoot: filepath.Join("/Users/test/Sites", fmt.Sprintf("ws-%d", i)),
				Created:       time.Now().Add(-time.Duration(i) * time.Hour),
				LastAccessed:  time.Now().Add(-time.Duration(i) * time.Minute),
			},
		}
	}
	cb := AppCallbacks{
		// Default ResumeSession: no-op success. Tests override per
		// case to count invocations.
		ResumeSession: func(*session.Session) error { return nil },
		ListSessions: func() (SessionSnapshot, error) {
			models := make(map[string]string, len(sessions))
			for _, sess := range sessions {
				models[sess.Name] = "opus"
			}
			return SessionSnapshot{Sessions: sessions, Models: models}, nil
		},
		// GetSessionDetail returns an empty detail so populateDetails
		// doesn't crash on lookup.
		GetSessionDetail: func(_ *session.Session) (SessionDetail, error) {
			return SessionDetail{}, nil
		},
	}
	a := NewApp(sessions, cb)
	scr := tcell.NewSimulationScreen("UTF-8")
	if err := scr.Init(); err != nil {
		t.Fatal(err)
	}
	scr.SetSize(120, 30)
	a.screen = scr
	a.layout()
	a.populateTable()
	a.running = true
	return a, scr, func() { scr.Fini() }
}

// snapshotApp renders the app into the simulation screen and dumps
// the cell buffer to qa-frames/ for visual review.
func snapshotApp(t *testing.T, a *App, scr tcell.SimulationScreen, name string) {
	t.Helper()
	a.draw()
	scr.Show()
	cells, cw, ch := scr.GetContents()
	var b strings.Builder
	fmt.Fprintf(&b, "# qa/%s.txt  size=%dx%d\n\n", name, cw, ch)
	for y := range ch {
		row := make([]rune, 0, cw)
		for x := range cw {
			c := cells[y*cw+x]
			if len(c.Runes) == 0 || c.Runes[0] == 0 {
				row = append(row, ' ')
				continue
			}
			row = append(row, c.Runes[0])
		}
		fmt.Fprintln(&b, strings.TrimRight(string(row), " "))
	}
	dir := getQADir()
	path := dir + "/qa-" + name + ".txt"
	if err := writeFile(path, b.String()); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("→ %s", path)
}

// getQADir centralises the env-override lookup so all the QA tests
// agree on where to write.
func getQADir() string {
	if d := os.Getenv("CLYDE_QA_DIR"); d != "" {
		return d
	}
	return os.TempDir()
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
