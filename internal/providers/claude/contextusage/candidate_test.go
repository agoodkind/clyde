package contextusage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/contextusage"
)

// TestCandidateProberRejectsMissingTranscriptPath verifies the input
// guards on the CountCandidate boundary. The Claude probe needs a real
// transcript path to derive the project dir, so empty input must fail
// loudly rather than spawn claude against a guessed location.
func TestCandidateProberRejectsMissingTranscriptPath(t *testing.T) {
	t.Parallel()
	_, err := claudeCandidateProber{}.CountCandidate(context.Background(), contextusage.CandidateRequest{
		LiveSessionID:  "session-1",
		TranscriptPath: "",
		WorkDir:        "/tmp",
		CandidateJSONL: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("CountCandidate with empty TranscriptPath returned nil error")
	}
	if !strings.Contains(err.Error(), "transcript") {
		t.Errorf("error %q does not mention transcript path", err)
	}
}

// TestCandidateProberRejectsEmptyJSONL ensures the probe refuses to
// write a zero-byte session file. Claude treats an empty session file
// as a broken resume target, so the guard surfaces the failure before
// any subprocess machinery starts.
func TestCandidateProberRejectsEmptyJSONL(t *testing.T) {
	t.Parallel()
	_, err := claudeCandidateProber{}.CountCandidate(context.Background(), contextusage.CandidateRequest{
		LiveSessionID:  "session-1",
		TranscriptPath: "/tmp/projects/encoded/session-1.jsonl",
		WorkDir:        "/tmp",
		CandidateJSONL: nil,
	})
	if err == nil {
		t.Fatal("CountCandidate with nil CandidateJSONL returned nil error")
	}
	if !strings.Contains(err.Error(), "candidate") {
		t.Errorf("error %q does not mention candidate jsonl", err)
	}
}

// TestWriteAndRemoveCandidateFile asserts the file-lifecycle helpers
// land bytes on disk, append a newline when missing, and remove the
// file cleanly. Cleaning up the disposable session file is a hard
// requirement of approach A so traces under ~/.claude/projects/ stay
// bounded.
func TestWriteAndRemoveCandidateFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "candidate.jsonl")
	body := []byte(`{"type":"user"}`)
	if err := writeCandidateFile(context.Background(), path, body); err != nil {
		t.Fatalf("writeCandidateFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read candidate file: %v", err)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("candidate file missing trailing newline: %q", got)
	}
	removeCandidateFile(context.Background(), path)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("candidate file not removed: stat err = %v", statErr)
	}
}

// TestCandidateProberRegistered confirms init() wires the Claude
// CandidateProber into the generic registry so the compact planner
// resolves it through contextusage.GetCandidate at runtime.
func TestCandidateProberRegistered(t *testing.T) {
	t.Parallel()
	prober, ok := contextusage.GetCandidate("claude")
	if !ok {
		t.Fatal("claude CandidateProber not registered")
	}
	if prober == nil {
		t.Fatal("registered CandidateProber is nil")
	}
}
