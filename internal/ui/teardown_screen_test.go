package ui

import (
	"sync/atomic"
	"testing"

	"github.com/gdamore/tcell/v2"
)

// finiTrackingScreen wraps a tcell.Screen and records the order of
// the cleanup calls teardownScreen performs. The test verifies that
// teardownScreen disables mouse, disables focus, and calls Fini
// before returning so the host terminal does not stay in alt-screen
// mode after the TUI exits.
type finiTrackingScreen struct {
	tcell.Screen
	disableMouseCount atomic.Int32
	disableFocusCount atomic.Int32
	finiCount         atomic.Int32
	calls             []string
}

func (s *finiTrackingScreen) DisableMouse() {
	s.disableMouseCount.Add(1)
	s.calls = append(s.calls, "DisableMouse")
	s.Screen.DisableMouse()
}

func (s *finiTrackingScreen) DisableFocus() {
	s.disableFocusCount.Add(1)
	s.calls = append(s.calls, "DisableFocus")
	s.Screen.DisableFocus()
}

func (s *finiTrackingScreen) Fini() {
	s.finiCount.Add(1)
	s.calls = append(s.calls, "Fini")
	s.Screen.Fini()
}

// TestTeardownScreenCallsFiniBeforeReturning is the regression test
// for the user report that exiting the TUI corrupts the terminal.
// Without screen.Fini(), tcell leaves the terminal in alt-screen
// mode and the user sees garbled output until they run `reset`.
func TestTeardownScreenCallsFiniBeforeReturning(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("sim screen init: %v", err)
	}
	tracker := &finiTrackingScreen{Screen: sim}

	app := NewApp(nil, AppCallbacks{})
	app.screen = tracker

	app.teardownScreen()

	if got := tracker.finiCount.Load(); got != 1 {
		t.Fatalf("expected Fini to be called exactly once, got %d", got)
	}
	if got := tracker.disableMouseCount.Load(); got != 1 {
		t.Fatalf("expected DisableMouse to be called once, got %d", got)
	}
	if got := tracker.disableFocusCount.Load(); got != 1 {
		t.Fatalf("expected DisableFocus to be called once, got %d", got)
	}

	// The order matters: tcell will keep emitting tracking
	// sequences if Fini happens before the disables.
	wantOrder := []string{"DisableMouse", "DisableFocus", "Fini"}
	if len(tracker.calls) < len(wantOrder) {
		t.Fatalf("expected at least %d cleanup calls, got %v", len(wantOrder), tracker.calls)
	}
	for i, want := range wantOrder {
		if tracker.calls[i] != want {
			t.Fatalf("cleanup call %d = %q, want %q (full sequence: %v)",
				i, tracker.calls[i], want, tracker.calls)
		}
	}

	// teardownScreen must be safe to call twice; subsequent calls
	// observe a nil screen and short-circuit without panicking.
	app.teardownScreen()
	if got := tracker.finiCount.Load(); got != 1 {
		t.Fatalf("expected Fini to remain at 1 after second teardown, got %d", got)
	}
}
