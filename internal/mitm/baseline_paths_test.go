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

func TestFindBaselineReferenceReturnsBaselineFile(t *testing.T) {
	root := t.TempDir()
	upstream := "claude-code"
	if err := os.MkdirAll(filepath.Join(root, upstream), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ref := BaselineReferencePath(root, upstream)
	if filepath.Base(ref) != "baseline-reference.toml" {
		t.Fatalf("BaselineReferencePath base=%q want baseline-reference.toml", filepath.Base(ref))
	}
	if err := os.WriteFile(ref, []byte("baseline"), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	got, err := FindBaselineReference(root, upstream)
	if err != nil {
		t.Fatalf("FindBaselineReference: %v", err)
	}
	if got != ref {
		t.Fatalf("FindBaselineReference()=%q want %q", got, ref)
	}
}

func TestFindBaselineReferenceMissingErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := FindBaselineReference(root, "claude-code"); err == nil {
		t.Fatal("FindBaselineReference: want error for missing baseline, got nil")
	}
}
