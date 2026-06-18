package mitm

import (
	"path/filepath"
	"testing"
)

func TestDefaultHookStagingRootUsesXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	got := DefaultHookStagingRoot()
	want := filepath.Join(dir, "clyde", "mitm-hooks")
	if got != want {
		t.Fatalf("DefaultHookStagingRoot()=%q want %q", got, want)
	}
}

func TestDefaultHookStagingRootExpandsXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "~/state/../state-root")

	stateRoot := filepath.Join(home, "state-root", "clyde")
	if got := DefaultHookStagingRoot(); got != filepath.Join(stateRoot, "mitm-hooks") {
		t.Fatalf("DefaultHookStagingRoot()=%q want %q", got, filepath.Join(stateRoot, "mitm-hooks"))
	}
}
