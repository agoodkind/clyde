package session_test

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/providers/claude/claudepath"
	"goodkind.io/clyde/internal/session"
)

// TestEffectiveTranscriptPathNilSession: nil session returns empty.
func TestEffectiveTranscriptPathNilSession(t *testing.T) {
	if got := session.EffectiveTranscriptPath(nil); got != "" {
		t.Fatalf("EffectiveTranscriptPath nil = %q, want empty", got)
	}
}

// TestEffectiveTranscriptPathClaudeRotatorLeakFallsBackToDerived: a Claude
// session whose stored transcriptPath points at a dead rotator dir is
// rerouted to the canonical SOT-derived path.
func TestEffectiveTranscriptPathClaudeRotatorLeakFallsBackToDerived(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspaceRoot := "/Users/test/Sites/proj"
	sessionID := "uuid-rotator-leak"
	derived := claudepath.TranscriptPathForWorkspace(home, workspaceRoot, sessionID)
	if err := os.MkdirAll(filepath.Dir(derived), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(derived, []byte("x"), 0o644); err != nil {
		t.Fatalf("write derived: %v", err)
	}
	dead := "/var/folders/x/T/clyde-501/rotator-launch/anthropic-99/projects/-Users-test-Sites-proj/" + sessionID + ".jsonl"

	sess := session.NewSession("test-session", sessionID)
	sess.Metadata.Provider = session.ProviderClaude
	sess.Metadata.WorkspaceRoot = workspaceRoot
	sess.Metadata.SetProviderTranscriptPath(dead)

	got := session.EffectiveTranscriptPath(sess)
	if got != derived {
		t.Fatalf("EffectiveTranscriptPath rotator-leak = %q, want %q", got, derived)
	}
}

// TestEffectiveTranscriptPathClaudeHealthyStoredKept: a healthy stored path
// passes through unchanged.
func TestEffectiveTranscriptPathClaudeHealthyStoredKept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspaceRoot := "/Users/test/Sites/proj"
	sessionID := "uuid-healthy"
	stored := filepath.Join(t.TempDir(), sessionID+".jsonl")
	if err := os.WriteFile(stored, []byte("x"), 0o644); err != nil {
		t.Fatalf("write stored: %v", err)
	}

	sess := session.NewSession("test-session", sessionID)
	sess.Metadata.Provider = session.ProviderClaude
	sess.Metadata.WorkspaceRoot = workspaceRoot
	sess.Metadata.SetProviderTranscriptPath(stored)

	got := session.EffectiveTranscriptPath(sess)
	if got != stored {
		t.Fatalf("EffectiveTranscriptPath healthy-stored = %q, want %q", got, stored)
	}
}

// TestEffectiveTranscriptPathCodexUnchanged: non-Claude providers get the
// stored path verbatim, no canonicalization.
func TestEffectiveTranscriptPathCodexUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stored := "/var/anywhere/codex.jsonl"

	sess := session.NewSession("test-session", "uuid-codex")
	sess.RotateIdentity(session.ProviderSessionID{Provider: session.ProviderCodex, ID: "uuid-codex"})
	sess.Metadata.WorkspaceRoot = "/Users/test/Sites/proj"
	sess.Metadata.SetProviderTranscriptPath(stored)
	if sess.ProviderID() != session.ProviderCodex {
		t.Fatalf("sess.ProviderID() = %q after setup, want %q", sess.ProviderID(), session.ProviderCodex)
	}

	got := session.EffectiveTranscriptPath(sess)
	if got != stored {
		t.Fatalf("EffectiveTranscriptPath codex = %q, want %q", got, stored)
	}
}
