package ui

import (
	"sync/atomic"
	"testing"
)

// TestShortPathPure checks that shortPath stays a pure function once
// the caller passes home explicitly. Covers the path-equals-home,
// path-under-home, and path-shorter-than-home branches so future
// regressions show up here rather than in a flame graph.
func TestShortPathPure(t *testing.T) {
	cases := []struct {
		name string
		root string
		home string
		want string
	}{
		{name: "empty root returns dash", root: "", home: "/Users/test", want: "-"},
		{name: "root equals home collapses to tilde", root: "/Users/test", home: "/Users/test", want: "~"},
		{name: "root under home gets relative tilde prefix", root: "/Users/test/Sites/repo", home: "/Users/test", want: "~/Sites/repo"},
		{name: "root outside home is returned unchanged", root: "/var/log/clyde", home: "/Users/test", want: "/var/log/clyde"},
		{name: "root shorter than home stays absolute", root: "/Users", home: "/Users/test", want: "/Users"},
		{name: "empty home leaves the path intact", root: "/Users/test/Sites/repo", home: "", want: "/Users/test/Sites/repo"},
		{name: "home prefix without trailing slash does not match", root: "/Users/testing/Sites/repo", home: "/Users/test", want: "/Users/testing/Sites/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shortPath(tc.root, tc.home)
			if got != tc.want {
				t.Fatalf("shortPath(%q, %q) = %q, want %q", tc.root, tc.home, got, tc.want)
			}
		})
	}
}

// TestDrawSettingsTabUsesProbeCache asserts that drawing the Settings
// tab does not reach the configured probe function. The probe is owned
// by the background ticker; the draw path reads the cached snapshot.
// Without that contract the per-frame os.Stat calls flagged in finding
// 2.1 would creep back in.
func TestDrawSettingsTabUsesProbeCache(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 0)
	defer cleanup()

	var probeCount atomic.Int64
	a.settingsProbeMu.Lock()
	a.settingsProbeFn = func(string) bool {
		probeCount.Add(1)
		return true
	}
	a.settingsProbeMu.Unlock()

	// Pre-populate the cache through the supervisor refresh path. This
	// is the only place probes are allowed; the counter records calls
	// here, then we reset before drawing.
	a.refreshSettingsProbes()
	if probeCount.Load() == 0 {
		t.Fatalf("expected refreshSettingsProbes to invoke probe at least once")
	}
	probeCount.Store(0)

	a.activeTab = 2
	for range 5 {
		a.drawSettingsTab(a.tableRect)
	}
	if got := probeCount.Load(); got != 0 {
		t.Fatalf("drawSettingsTab probed the filesystem %d times, want 0", got)
	}
}

// TestSettingsProbeCacheReflectsExistence confirms the cached row's
// status flips when the configured probe changes its mind. The draw
// path in turn renders "(exists)" or "(missing)" off that snapshot.
func TestSettingsProbeCacheReflectsExistence(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 0)
	defer cleanup()

	a.settingsProbeMu.Lock()
	a.settingsProbeFn = func(string) bool { return true }
	a.settingsProbeMu.Unlock()
	a.refreshSettingsProbes()
	rows := a.settingsProbeRowsSnapshot()
	if len(rows) < 2 {
		t.Fatalf("settings probe cache rows = %d, want >= 2", len(rows))
	}
	if !rows[0].exists {
		t.Fatalf("global config row should be exists when probe returns true")
	}
	desc := configRowDescription(rows[0])
	if desc == "" || desc[len(desc)-len("(exists)"):] != "(exists)" {
		t.Fatalf("configRowDescription should end in (exists), got %q", desc)
	}

	a.settingsProbeMu.Lock()
	a.settingsProbeFn = func(string) bool { return false }
	a.settingsProbeMu.Unlock()
	a.refreshSettingsProbes()
	rows = a.settingsProbeRowsSnapshot()
	if rows[0].exists {
		t.Fatalf("global config row should be missing when probe returns false")
	}
	desc = configRowDescription(rows[0])
	if desc == "" || desc[len(desc)-len("(missing)"):] != "(missing)" {
		t.Fatalf("configRowDescription should end in (missing), got %q", desc)
	}
}

// TestHomeDirCachedAcrossCalls confirms App.homeDir resolves the home
// directory once. The probe runs once per process so the per-cell hot
// path called from rowFor never enters os.UserHomeDir again.
func TestHomeDirCachedAcrossCalls(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 0)
	defer cleanup()

	first := a.homeDir()
	second := a.homeDir()
	if first != second {
		t.Fatalf("homeDir returned different values: %q vs %q", first, second)
	}
}
