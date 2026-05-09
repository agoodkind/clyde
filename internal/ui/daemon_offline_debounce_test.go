package ui

import (
	"errors"
	"testing"
	"time"
)

func newAppWithFakeClockForTest(t *testing.T, clock *fakeUIClock) *App {
	t.Helper()
	app := NewApp(nil, AppCallbacks{})
	app.clock = clock
	// Force the daemon into the "previously online" state so the
	// next setDaemonOffline records daemonOfflineSince via the
	// transition path.
	app.setDaemonOnline("test_seed")
	return app
}

// TestDaemonOfflineDebounceSuppressesTransientFlicker is the regression
// test for the user report that "registry stream closed" / "DAEMON
// offline" flashes during the registry supervisor's quick reconnects.
// A close-and-reopen that completes inside daemonOfflineDebounce must
// never flip the displayed state to offline.
func TestDaemonOfflineDebounceSuppressesTransientFlicker(t *testing.T) {
	clock := &fakeUIClock{t: time.Unix(1_700_000_000, 0)}
	app := newAppWithFakeClockForTest(t, clock)

	// Simulate the registry stream closing, the supervisor noticing,
	// and the subscribe call succeeding 200ms later. 200ms is well
	// inside the 500ms debounce window.
	app.setDaemonOffline("stream_closed", errors.New("registry stream closed"))
	if app.daemonOfflineSince.IsZero() {
		t.Fatal("expected daemonOfflineSince to be recorded on first offline")
	}
	if app.shouldDisplayDaemonOffline() {
		t.Fatal("expected debounce to suppress offline display immediately after transition")
	}

	clock.t = clock.t.Add(200 * time.Millisecond)
	if app.shouldDisplayDaemonOffline() {
		t.Fatal("expected debounce to still suppress offline display at 200ms")
	}

	app.setDaemonOnline("subscribe_ok")
	if app.shouldDisplayDaemonOffline() {
		t.Fatal("expected reconnect to clear offline display")
	}
	if !app.daemonOfflineSince.IsZero() {
		t.Fatal("expected daemonOfflineSince to reset on reconnect")
	}
}

// TestDaemonOfflineDisplaysAfterDebounce makes sure a real outage
// surfaces. We do not want the debounce to mask actual failures.
func TestDaemonOfflineDisplaysAfterDebounce(t *testing.T) {
	clock := &fakeUIClock{t: time.Unix(1_700_000_000, 0)}
	app := newAppWithFakeClockForTest(t, clock)

	app.setDaemonOffline("stream_closed", errors.New("registry stream closed"))
	clock.t = clock.t.Add(daemonOfflineDebounce + 50*time.Millisecond)
	if !app.shouldDisplayDaemonOffline() {
		t.Fatal("expected offline display once daemonOfflineDebounce elapses")
	}
}
