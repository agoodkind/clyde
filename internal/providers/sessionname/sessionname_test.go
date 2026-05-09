package sessionname

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/session"
)

// claudeRenameEntry captures the on-disk shape Claude rename appends emit.
// It mirrors the writer in claude.go without re-exporting the unexported types.
type claudeRenameEntry struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle,omitempty"`
	AgentName   string `json:"agentName,omitempty"`
	SessionID   string `json:"sessionId"`
}

func copyFixtureToTempFile(t *testing.T, fixture string) string {
	t.Helper()
	source := filepath.Join("testdata", fixture)
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read fixture %q: %v", source, err)
	}
	dest := filepath.Join(t.TempDir(), filepath.Base(fixture))
	if err := os.WriteFile(dest, body, 0o600); err != nil {
		t.Fatalf("write fixture copy %q: %v", dest, err)
	}
	return dest
}

func newClaudeSessionWithTranscript(t *testing.T, transcriptPath, sessionID string) *session.Session {
	t.Helper()
	return &session.Session{
		Name: "chat-1",
		Metadata: session.Metadata{
			Provider:       session.ProviderClaude,
			SessionID:      sessionID,
			TranscriptPath: transcriptPath,
		},
	}
}

func readClaudeRenameEntries(t *testing.T, path string) []claudeRenameEntry {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read transcript %q: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	entries := make([]claudeRenameEntry, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry := claudeRenameEntry{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse transcript line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// --- Rename entrypoint ----------------------------------------------------

func TestRenameRejectsNilSession(t *testing.T) {
	if err := Rename(context.Background(), nil, "name"); err == nil {
		t.Fatal("Rename(nil session) returned nil error")
	}
}

func TestRenameRejectsEmptyName(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")
	if err := Rename(context.Background(), sess, ""); err == nil {
		t.Fatal("Rename(empty) returned nil error")
	}
}

func TestRenameRejectsWhitespaceOnlyName(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")
	if err := Rename(context.Background(), sess, "   \t  "); err == nil {
		t.Fatal("Rename(whitespace) returned nil error")
	}
}

func TestRenameRejectsNameExceedingDisplayPolicy(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")
	tooLong := strings.Repeat("a", session.MaxDisplayNameLength+1)
	err := Rename(context.Background(), sess, tooLong)
	if err == nil {
		t.Fatal("Rename(too long) returned nil error")
	}
	if !errors.Is(err, session.ErrInvalidDisplayName) {
		t.Fatalf("Rename(too long) err = %v, want errors.Is ErrInvalidDisplayName", err)
	}
}

func TestRenameRejectsControlCharacterName(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")
	err := Rename(context.Background(), sess, "bad\x00name")
	if err == nil {
		t.Fatal("Rename(control char) returned nil error")
	}
	if !errors.Is(err, session.ErrInvalidDisplayName) {
		t.Fatalf("Rename(control char) err = %v, want errors.Is ErrInvalidDisplayName", err)
	}
}

func TestRenameRejectsUnknownProvider(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")
	sess.Metadata.Provider = session.ProviderID("weird")
	err := Rename(context.Background(), sess, "Renamed")
	if err == nil {
		t.Fatal("Rename(unknown provider) returned nil error")
	}
	if !strings.Contains(err.Error(), "unsupported session provider") {
		t.Fatalf("Rename(unknown provider) err = %v, want unsupported session provider", err)
	}
}

func TestRenameDispatchesToClaudeWriter(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")

	if err := Rename(context.Background(), sess, "Renamed Title"); err != nil {
		t.Fatalf("Rename returned error: %v", err)
	}

	entries := readClaudeRenameEntries(t, transcript)
	if len(entries) < 2 {
		t.Fatalf("Rename produced %d entries, want >= 2", len(entries))
	}
	last := entries[len(entries)-2:]
	if last[0].Type != "custom-title" || last[0].CustomTitle != "Renamed Title" {
		t.Fatalf("custom-title entry = %+v, want type=custom-title customTitle=Renamed Title", last[0])
	}
	if last[1].Type != "agent-name" || last[1].AgentName != "Renamed Title" {
		t.Fatalf("agent-name entry = %+v, want type=agent-name agentName=Renamed Title", last[1])
	}
	if last[0].SessionID != "session-baseline" || last[1].SessionID != "session-baseline" {
		t.Fatalf("rename entries sessionId mismatch: %+v", last)
	}
}

func TestRenameTrimsLeadingTrailingWhitespace(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")

	if err := Rename(context.Background(), sess, "  Padded Title  "); err != nil {
		t.Fatalf("Rename returned error: %v", err)
	}

	entries := readClaudeRenameEntries(t, transcript)
	if len(entries) < 2 {
		t.Fatalf("Rename produced %d entries, want >= 2", len(entries))
	}
	last := entries[len(entries)-2:]
	if last[0].CustomTitle != "Padded Title" {
		t.Fatalf("custom-title trimmed value = %q, want %q", last[0].CustomTitle, "Padded Title")
	}
}

// TestRenameToExistingNameAppendsAgain documents the current behavior:
// renaming to the existing title is not a no-op. The implementation
// always appends fresh custom-title and agent-name entries. If the
// package later adds a sentinel for idempotent renames, this test should
// be updated to reflect that contract.
func TestRenameToExistingNameAppendsAgain(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_with_existing_name.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-existing")

	beforeEntries := readClaudeRenameEntries(t, transcript)
	if err := Rename(context.Background(), sess, "Existing Title"); err != nil {
		t.Fatalf("Rename returned error: %v", err)
	}
	afterEntries := readClaudeRenameEntries(t, transcript)
	if len(afterEntries) != len(beforeEntries)+2 {
		t.Fatalf("len(after) = %d, len(before) = %d, want before+2", len(afterEntries), len(beforeEntries))
	}
}

// --- renameClaude --------------------------------------------------------

func TestRenameClaudeRejectsMissingTranscriptPath(t *testing.T) {
	sess := &session.Session{
		Name: "chat-1",
		Metadata: session.Metadata{
			Provider:  session.ProviderClaude,
			SessionID: "session-1",
		},
	}
	if err := renameClaude(sess, "Renamed"); err == nil {
		t.Fatal("renameClaude(missing transcript) returned nil error")
	}
}

func TestRenameClaudeRejectsMissingSessionID(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := &session.Session{
		Name: "chat-1",
		Metadata: session.Metadata{
			Provider:       session.ProviderClaude,
			TranscriptPath: transcript,
		},
	}
	if err := renameClaude(sess, "Renamed"); err == nil {
		t.Fatal("renameClaude(missing session id) returned nil error")
	}
}

func TestRenameClaudeReturnsErrorForMissingTranscriptFile(t *testing.T) {
	sess := newClaudeSessionWithTranscript(t,
		filepath.Join(t.TempDir(), "does-not-exist.jsonl"),
		"session-baseline",
	)
	err := renameClaude(sess, "Renamed")
	if err == nil {
		t.Fatal("renameClaude(missing file) returned nil error")
	}
	if !strings.Contains(err.Error(), "open claude transcript for rename") {
		t.Fatalf("renameClaude err = %v, want wrapper open claude transcript for rename", err)
	}
}

func TestRenameClaudeWrapsReadOnlyTranscriptError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses unix permission bits")
	}
	dir := t.TempDir()
	transcript := filepath.Join(dir, "ro.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"type":"system","sessionId":"session-ro"}`+"\n"), 0o400); err != nil {
		t.Fatalf("write read-only transcript: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(transcript, 0o600)
	})

	sess := newClaudeSessionWithTranscript(t, transcript, "session-ro")
	err := renameClaude(sess, "Renamed")
	if err == nil {
		t.Fatal("renameClaude(read-only) returned nil error")
	}
	if !strings.Contains(err.Error(), "open claude transcript for rename") {
		t.Fatalf("renameClaude err = %v, want wrapper open claude transcript for rename", err)
	}
}

// TestRenameClaudeAppendsToTranscript covers the on-disk content shape
// directly through the unexported writer, mirroring the existing fixture
// shape from internal/providers/claude/lifecycle.
func TestRenameClaudeAppendsToTranscript(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")

	if err := renameClaude(sess, "Renamed Direct"); err != nil {
		t.Fatalf("renameClaude returned error: %v", err)
	}

	body, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `"type":"custom-title"`) || !strings.Contains(got, `"customTitle":"Renamed Direct"`) {
		t.Fatalf("transcript missing custom-title entry: %s", got)
	}
	if !strings.Contains(got, `"type":"agent-name"`) || !strings.Contains(got, `"agentName":"Renamed Direct"`) {
		t.Fatalf("transcript missing agent-name entry: %s", got)
	}
}

// TestRenameClaudeConcurrentAppendsKeepValidJSONL exercises the concurrent
// rename path. The current implementation relies on POSIX O_APPEND atomic
// writes for whole-line entries; this test asserts that property: the file
// remains a valid JSONL stream and contains the expected number of new
// entries after concurrent calls.
func TestRenameClaudeConcurrentAppendsKeepValidJSONL(t *testing.T) {
	transcript := copyFixtureToTempFile(t, "claude_transcript_baseline.jsonl")
	sess := newClaudeSessionWithTranscript(t, transcript, "session-baseline")

	const concurrency = 8
	var waitGroup sync.WaitGroup
	errs := make(chan error, concurrency)
	waitGroup.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer waitGroup.Done()
			errs <- renameClaude(sess, "Concurrent Title")
		}()
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("renameClaude returned error: %v", err)
		}
	}

	entries := readClaudeRenameEntries(t, transcript)
	customCount := 0
	agentCount := 0
	for _, entry := range entries {
		if entry.Type == "custom-title" && entry.CustomTitle == "Concurrent Title" {
			customCount++
		}
		if entry.Type == "agent-name" && entry.AgentName == "Concurrent Title" {
			agentCount++
		}
	}
	if customCount != concurrency {
		t.Fatalf("custom-title count = %d, want %d", customCount, concurrency)
	}
	if agentCount != concurrency {
		t.Fatalf("agent-name count = %d, want %d", agentCount, concurrency)
	}
}
