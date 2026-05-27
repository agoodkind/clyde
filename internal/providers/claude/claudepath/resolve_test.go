package claudepath_test

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/providers/claude/claudepath"
)

// TestResolveTranscriptPathPrefersStoredWhenAlive: existing files keep their
// stored path so operator-edited paths and historical placements still work.
func TestResolveTranscriptPathPrefersStoredWhenAlive(t *testing.T) {
	home := t.TempDir()
	stored := filepath.Join(t.TempDir(), "alive.jsonl")
	if err := os.WriteFile(stored, []byte("x"), 0o644); err != nil {
		t.Fatalf("write stored: %v", err)
	}
	got := claudepath.ResolveTranscriptPath(home, stored, "/Users/test/proj", "uuid-1")
	if got != stored {
		t.Fatalf("ResolveTranscriptPath stored-alive = %q, want %q", got, stored)
	}
}

// TestResolveTranscriptPathFallsBackToDerivedWhenStoredDead: a dead stored
// path triggers the canonical derivation from workspaceRoot + UUID.
func TestResolveTranscriptPathFallsBackToDerivedWhenStoredDead(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := "/Users/test/Sites/proj"
	sessionID := "uuid-1"
	derived := claudepath.TranscriptPathForWorkspace(home, workspaceRoot, sessionID)
	if err := os.MkdirAll(filepath.Dir(derived), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(derived, []byte("x"), 0o644); err != nil {
		t.Fatalf("write derived: %v", err)
	}
	stored := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-99/projects/-Users-test-Sites-proj/" + sessionID + ".jsonl"

	got := claudepath.ResolveTranscriptPath(home, stored, workspaceRoot, sessionID)
	if got != derived {
		t.Fatalf("ResolveTranscriptPath stored-dead = %q, want %q", got, derived)
	}
}

// TestResolveTranscriptPathReturnsDerivedWhenBothMissing: when neither file
// exists, derived wins because it is where claude will write next.
func TestResolveTranscriptPathReturnsDerivedWhenBothMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := "/Users/test/Sites/proj"
	sessionID := "uuid-1"
	stored := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-99/projects/-Users-test-Sites-proj/" + sessionID + ".jsonl"

	got := claudepath.ResolveTranscriptPath(home, stored, workspaceRoot, sessionID)
	expected := claudepath.TranscriptPathForWorkspace(home, workspaceRoot, sessionID)
	if got != expected {
		t.Fatalf("ResolveTranscriptPath both-missing = %q, want %q", got, expected)
	}
}

// TestResolveTranscriptPathReturnsStoredWhenNoWorkspaceRoot: without a
// workspaceRoot we cannot derive, so the stored path passes through.
func TestResolveTranscriptPathReturnsStoredWhenNoWorkspaceRoot(t *testing.T) {
	home := t.TempDir()
	stored := "/var/folders/x/dead.jsonl"

	got := claudepath.ResolveTranscriptPath(home, stored, "", "uuid-1")
	if got != stored {
		t.Fatalf("ResolveTranscriptPath no-workspace = %q, want %q", got, stored)
	}
}

// TestResolveTranscriptPathReturnsEmptyWhenAllInputsEmpty: every input empty
// leaves nothing to return.
func TestResolveTranscriptPathReturnsEmptyWhenAllInputsEmpty(t *testing.T) {
	got := claudepath.ResolveTranscriptPath("", "", "", "")
	if got != "" {
		t.Fatalf("ResolveTranscriptPath empty = %q, want empty", got)
	}
}

// TestResolveTranscriptPathReturnsDerivedWhenStoredEmptyButDerivableAndMissing:
// no stored but derivable inputs still return the canonical path so the caller
// has something to try.
func TestResolveTranscriptPathReturnsDerivedWhenStoredEmptyButDerivableAndMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := "/Users/test/Sites/proj"
	sessionID := "uuid-1"

	got := claudepath.ResolveTranscriptPath(home, "", workspaceRoot, sessionID)
	expected := claudepath.TranscriptPathForWorkspace(home, workspaceRoot, sessionID)
	if got != expected {
		t.Fatalf("ResolveTranscriptPath empty-stored = %q, want %q", got, expected)
	}
}
