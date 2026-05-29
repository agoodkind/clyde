package mitm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultBaselineRootUsesXDGStateHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	got := DefaultBaselineRoot()
	want := filepath.Join(dir, "clyde", "mitm-baselines")
	if got != want {
		t.Fatalf("DefaultBaselineRoot()=%q want %q", got, want)
	}
}

func TestDefaultMITMStateRootsExpandXDGStateHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "~/state/../state-root")

	stateRoot := filepath.Join(home, "state-root", "clyde")
	if got := DefaultBaselineRoot(); got != filepath.Join(stateRoot, "mitm-baselines") {
		t.Fatalf("DefaultBaselineRoot()=%q want %q", got, filepath.Join(stateRoot, "mitm-baselines"))
	}
	if got := DefaultHookStagingRoot(); got != filepath.Join(stateRoot, "mitm-hooks") {
		t.Fatalf("DefaultHookStagingRoot()=%q want %q", got, filepath.Join(stateRoot, "mitm-hooks"))
	}
	if got := DefaultDriftLogDir(); got != filepath.Join(stateRoot, "mitm-drift") {
		t.Fatalf("DefaultDriftLogDir()=%q want %q", got, filepath.Join(stateRoot, "mitm-drift"))
	}
	if got := DefaultCaptureRoot(); got != filepath.Join(stateRoot, "mitm") {
		t.Fatalf("DefaultCaptureRoot()=%q want %q", got, filepath.Join(stateRoot, "mitm"))
	}
}

func TestFindBaselineReferencePrefersV2(t *testing.T) {
	root := t.TempDir()
	upstream := "claude-code"
	if err := os.MkdirAll(filepath.Join(root, upstream), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	v1 := BaselineReferencePath(root, upstream, false)
	v2 := BaselineReferencePath(root, upstream, true)
	if err := os.WriteFile(v1, []byte("v1"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if err := os.WriteFile(v2, []byte("v2"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	got, err := FindBaselineReference(root, upstream)
	if err != nil {
		t.Fatalf("FindBaselineReference: %v", err)
	}
	if got != v2 {
		t.Fatalf("FindBaselineReference()=%q want %q", got, v2)
	}
}
