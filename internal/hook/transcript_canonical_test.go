package hook

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/providers/claude/claudepath"
)

// TestCanonicalTranscriptPathEmptyReportedReturnsEmpty: the writer skips when
// nothing was reported.
func TestCanonicalTranscriptPathEmptyReportedReturnsEmpty(t *testing.T) {
	got := canonicalTranscriptPath("/home/user", "", "/proj", "uuid-1")
	if got != "" {
		t.Fatalf("canonicalTranscriptPath empty reported = %q, want empty", got)
	}
}

// TestCanonicalTranscriptPathNoHomeReturnsReported: with no home we cannot
// canonicalize, so the original is preserved.
func TestCanonicalTranscriptPathNoHomeReturnsReported(t *testing.T) {
	reported := "/var/x/projects/-proj/uuid-1.jsonl"
	got := canonicalTranscriptPath("", reported, "/proj", "uuid-1")
	if got != reported {
		t.Fatalf("canonicalTranscriptPath no-home = %q, want %q", got, reported)
	}
}

// TestCanonicalTranscriptPathReportedAlreadyCanonical: paths already under
// <home>/.claude/projects/ are stored verbatim.
func TestCanonicalTranscriptPathReportedAlreadyCanonical(t *testing.T) {
	home := t.TempDir()
	reported := filepath.Join(home, ".claude", "projects", "-proj", "uuid-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(reported), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := canonicalTranscriptPath(home, reported, "/proj", "uuid-1")
	if got != reported {
		t.Fatalf("canonicalTranscriptPath canonical-in = %q, want %q", got, reported)
	}
}

// TestCanonicalTranscriptPathDerivedWhenDerivedExists: a rotator-prefixed
// reportedPath is replaced by the canonical workspaceRoot+UUID path when that
// canonical file exists on disk.
func TestCanonicalTranscriptPathDerivedWhenDerivedExists(t *testing.T) {
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
	reported := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-12345/projects/-Users-test-Sites-proj/" + sessionID + ".jsonl"

	got := canonicalTranscriptPath(home, reported, workspaceRoot, sessionID)
	if got != derived {
		t.Fatalf("canonicalTranscriptPath rotator-with-derived = %q, want %q", got, derived)
	}
}

// TestCanonicalTranscriptPathDerivedFallbackWhenDerivedMissing: derived path is
// still selected when the file does not yet exist on disk, since the file may
// appear after the hook fires.
func TestCanonicalTranscriptPathDerivedFallbackWhenDerivedMissing(t *testing.T) {
	home := t.TempDir()
	workspaceRoot := "/Users/test/Sites/proj"
	sessionID := "uuid-1"
	reported := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-12345/projects/-Users-test-Sites-proj/" + sessionID + ".jsonl"

	got := canonicalTranscriptPath(home, reported, workspaceRoot, sessionID)
	expected := claudepath.TranscriptPathForWorkspace(home, workspaceRoot, sessionID)
	if got != expected {
		t.Fatalf("canonicalTranscriptPath rotator-no-derived = %q, want %q", got, expected)
	}
}

// TestCanonicalTranscriptPathSymlinkResolution: when no workspaceRoot is
// available but the reported path resolves through a symlink chain back into
// the real <home>/.claude/projects/ tree, the resolved target is used.
func TestCanonicalTranscriptPathSymlinkResolution(t *testing.T) {
	home := t.TempDir()
	realFile := filepath.Join(home, ".claude", "projects", "-proj", "uuid-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(realFile), 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	if err := os.WriteFile(realFile, []byte("x"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	rotatorRoot := filepath.Join(t.TempDir(), "rotator-launch", "anthropic-12345")
	if err := os.MkdirAll(rotatorRoot, 0o755); err != nil {
		t.Fatalf("mkdir rotator: %v", err)
	}
	if err := os.Symlink(filepath.Join(home, ".claude", "projects"), filepath.Join(rotatorRoot, "projects")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	reported := filepath.Join(rotatorRoot, "projects", "-proj", "uuid-1.jsonl")

	got := canonicalTranscriptPath(home, reported, "", "uuid-1")
	resolved, err := filepath.EvalSymlinks(realFile)
	if err != nil {
		t.Fatalf("eval real: %v", err)
	}
	if got != resolved {
		t.Fatalf("canonicalTranscriptPath symlink-resolve = %q, want %q", got, resolved)
	}
}

// TestCanonicalTranscriptPathReportedAsLastResort: when neither workspaceRoot
// nor symlink resolution help, the reported path passes through.
func TestCanonicalTranscriptPathReportedAsLastResort(t *testing.T) {
	home := t.TempDir()
	reported := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-12345/projects/-proj/uuid-1.jsonl"

	got := canonicalTranscriptPath(home, reported, "", "uuid-1")
	if got != reported {
		t.Fatalf("canonicalTranscriptPath last-resort = %q, want %q", got, reported)
	}
}

// TestPathUnderDirRejectsSiblingPrefix: a sibling directory sharing a string
// prefix is not under the target directory.
func TestPathUnderDirRejectsSiblingPrefix(t *testing.T) {
	if pathUnderDir("/home/user-other/file", "/home/user") {
		t.Fatal("pathUnderDir false-positive on prefix-sibling")
	}
}

// TestPathUnderDirAcceptsExactAndNested: equality and nested descent both pass.
func TestPathUnderDirAcceptsExactAndNested(t *testing.T) {
	if !pathUnderDir("/home/user", "/home/user") {
		t.Fatal("pathUnderDir rejected exact match")
	}
	if !pathUnderDir("/home/user/sub/file", "/home/user") {
		t.Fatal("pathUnderDir rejected nested descent")
	}
}
