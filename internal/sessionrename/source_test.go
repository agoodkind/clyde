package sessionrename

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/session"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestExtractFromTranscriptPrefersCustomTitle(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"first message"}}`,
		`{"type":"custom-title","customTitle":"Refactor rename funnel"}`,
	)
	src, err := extractFromClaudeTranscript(path)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if src.Kind != sourceCustomTitle {
		t.Fatalf("kind=%q", src.Kind)
	}
	if src.Text != "Refactor rename funnel" {
		t.Fatalf("text=%q", src.Text)
	}
	if src.UserMsgCount != 1 {
		t.Fatalf("user_msg_count=%d", src.UserMsgCount)
	}
}

func TestExtractFromTranscriptFallsBackToFirstUserMessage(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"add retry to the bridge handler"}}`,
		`{"type":"user","timestamp":"2025-01-01T00:01:00Z","message":{"role":"user","content":"second message"}}`,
	)
	src, err := extractFromClaudeTranscript(path)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if src.Kind != sourceFirstUserMessage {
		t.Fatalf("kind=%q", src.Kind)
	}
	if !strings.Contains(src.Text, "add retry to the bridge handler") {
		t.Fatalf("text=%q", src.Text)
	}
	if src.UserMsgCount != 2 {
		t.Fatalf("user_msg_count=%d", src.UserMsgCount)
	}
}

func TestExtractFromTranscriptEmptyReturnsErr(t *testing.T) {
	path := writeTranscript(t)
	if _, err := extractFromClaudeTranscript(path); err == nil {
		t.Fatalf("expected err")
	}
}

func TestExtractFromTranscriptHandlesContentBlocks(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello world"}]}}`,
	)
	src, err := extractFromClaudeTranscript(path)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if src.Text != "hello world" {
		t.Fatalf("text=%q", src.Text)
	}
}

func TestExtractFromTranscriptHashIsStable(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"custom-title","customTitle":"Rename funnel"}`,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
	)
	a, err := extractFromClaudeTranscript(path)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	b, err := extractFromClaudeTranscript(path)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if a.Hash != b.Hash {
		t.Fatalf("hashes differ: %q vs %q", a.Hash, b.Hash)
	}
	if a.Hash == "" {
		t.Fatalf("hash empty")
	}
}

func TestExtractFromSessionUsesCodexHistory(t *testing.T) {
	path := writeTranscript(t,
		`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`,
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"please trace the exact codex auto name diff"}]}}`,
		`{"timestamp":"2026-05-02T17:09:06.000Z","type":"event_msg","payload":{"type":"agent_message","message":"Looking.","phase":"commentary"}}`,
		`{"timestamp":"2026-05-02T17:09:07.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"second user turn"}]}}`,
	)
	sess := session.NewSession("repo-deadbeef", "codex-thread")
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{
			Provider: session.ProviderCodex,
			ID:       "codex-thread",
		},
	}
	sess.Metadata.SetProviderTranscriptPath(path)

	src, err := extractFromSession(sess)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if src.Kind != sourceFirstUserMessage {
		t.Fatalf("kind=%q", src.Kind)
	}
	if src.UserMsgCount != 2 {
		t.Fatalf("user_msg_count=%d", src.UserMsgCount)
	}
	if src.Text != "please trace the exact codex auto name diff" {
		t.Fatalf("text=%q", src.Text)
	}
	if src.Hash == "" {
		t.Fatalf("hash empty")
	}
}
