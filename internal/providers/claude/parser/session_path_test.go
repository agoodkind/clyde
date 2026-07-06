package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptPathForSessionFindsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessionID := "abc-123-session"
	projectDir := filepath.Join(home, ".claude", "projects", "-some-encoded-cwd")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	want := filepath.Join(projectDir, sessionID+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	got, ok := TranscriptPathForSession(sessionID)
	if !ok || got != want {
		t.Fatalf("TranscriptPathForSession = %q, %v; want %q, true", got, ok, want)
	}
}

func TestTranscriptPathForSessionSkipsSubagents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sessionID := "sub-session"
	subDir := filepath.Join(home, ".claude", "projects", "proj", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir subagents dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subDir, sessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write subagent transcript: %v", err)
	}
	if _, ok := TranscriptPathForSession(sessionID); ok {
		t.Fatal("a subagent transcript must not resolve a session path")
	}
}

func TestTranscriptPathForSessionMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, ok := TranscriptPathForSession("no-such-session"); ok {
		t.Fatal("a missing session must return false")
	}
	if _, ok := TranscriptPathForSession(""); ok {
		t.Fatal("an empty session id must return false")
	}
}
