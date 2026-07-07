//go:build live

package live

import (
	"testing"
	"time"
)

// TestPreflightRejectsOccupiedPort asserts the harness refuses to boot when the
// fake MITM port is already bound, so a live run can never collide with another
// listener. This test never boots a daemon.
func TestPreflightRejectsOccupiedPort(t *testing.T) {
	h := newHarness(t)
	stop := h.occupyPort(t, h.cfg.MITMPort)
	defer stop()
	if err := h.preflight(); err == nil {
		t.Fatal("preflight must fail when the fake MITM port is already listening")
	}
}

// TestLiveHotApplyNoReexec boots the worktree daemon on the fake MITM port,
// edits a hot config field (the MITM providers list), and asserts the daemon
// applies it in process (daemon.config.applied_in_process) without re-execing
// (no daemon.config_watch.reload_triggered). Production daemons are untouched.
func TestLiveHotApplyNoReexec(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, h.cfg.MITMPort, []string{"anthropic"})
	h.boot(t)

	// Edit a hot field: extend the providers list. This is in the classifier's
	// hot set, so the daemon swaps proxy routing in process.
	h.writeConfig(t, h.cfg.MITMPort, []string{"anthropic", "codex"})

	if !h.waitForDaemonLog(hotApplyMarker, 10*time.Second) {
		t.Fatalf("hot apply did not occur (no %q)", hotApplyMarker)
	}
	if h.logContains(reloadTriggeredKey) {
		t.Fatalf("hot apply unexpectedly re-execed (saw %q)", reloadTriggeredKey)
	}
}

// TestLiveTopologyChangeReexecs boots the daemon, edits the MITM listener port
// (a topology change), and asserts the daemon routes to a reload/rebind
// (daemon.config_watch.reload_triggered) rather than a hot apply. Production
// daemons are untouched.
func TestLiveTopologyChangeReexecs(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, h.cfg.MITMPort, []string{"anthropic"})
	h.boot(t)

	// Move the MITM listener to a different fake port: a topology change.
	movedPort := h.cfg.MovedMITMPort
	if portListening(movedPort) {
		t.Skipf("moved port %d already in use; skipping", movedPort)
	}
	h.writeConfig(t, movedPort, []string{"anthropic"})

	// The classifier routes a listener move to rebind, and the daemon enters the
	// rebind drain. Asserting both proves the topology change takes the
	// zero-bind-gap re-exec path rather than a hot apply; the full rebind
	// machinery is exercised by the daemon package's own reload tests.
	if !h.waitForDaemonLog(`"route":"rebind"`, 15*time.Second) {
		t.Fatalf("topology change was not classified as rebind")
	}
	if !h.waitForDaemonLog("daemon.rebind.draining_listeners", 10*time.Second) {
		t.Fatalf("topology change did not enter the rebind drain")
	}
	if h.logContains(hotApplyMarker) {
		t.Fatalf("topology change unexpectedly hot-applied (saw %q)", hotApplyMarker)
	}
}
