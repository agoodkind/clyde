package hook

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/session"
)

func TestProcessSessionStartAutoAdoptUsesLaunchCWD(t *testing.T) {
	t.Setenv("CLYDE_SESSION_NAME", "chat-test")

	store := session.NewFileStore(t.TempDir())
	launchCWD := filepath.Join(t.TempDir(), "repo", "nested")
	projectRoot := filepath.Dir(launchCWD)
	t.Setenv("CLYDE_LAUNCH_CWD", launchCWD)

	input := bytes.NewBufferString(`{"session_id":"test-uuid","source":"startup","transcript_path":"/tmp/test-uuid.jsonl"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		Getwd: func() (string, error) {
			return projectRoot, nil
		},
		FindProjectRoot: func() (string, error) {
			return projectRoot, nil
		},
		LogRawEvent: func([]byte, string) error {
			return nil
		},
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}

	sess, err := store.Get("chat-test")
	if err != nil {
		t.Fatalf("Get adopted session: %v", err)
	}
	if sess.Metadata.WorkDir != launchCWD {
		t.Fatalf("WorkDir = %q, want %q", sess.Metadata.WorkDir, launchCWD)
	}
	if sess.Metadata.WorkspaceRoot != launchCWD {
		t.Fatalf("WorkspaceRoot = %q, want %q", sess.Metadata.WorkspaceRoot, launchCWD)
	}
}

func TestProcessSessionStartAutoAdoptUsesTranscriptCWD(t *testing.T) {
	t.Setenv("CLYDE_SESSION_NAME", "chat-test-transcript")
	t.Setenv("CLYDE_LAUNCH_CWD", "")

	store := session.NewFileStore(t.TempDir())
	workspace := filepath.Join(t.TempDir(), "repo", "nested")
	projectRoot := filepath.Dir(workspace)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	body := `{"type":"system","timestamp":"2026-04-26T18:00:00Z","entrypoint":"cli","cwd":"` + workspace + `","sessionId":"test-uuid-transcript"}` + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
		t.Fatalf("Write transcript: %v", err)
	}

	input := bytes.NewBufferString(`{"session_id":"test-uuid-transcript","source":"startup","transcript_path":"` + transcript + `"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		Getwd: func() (string, error) {
			return projectRoot, nil
		},
		FindProjectRoot: func() (string, error) {
			return projectRoot, nil
		},
		LogRawEvent: func([]byte, string) error {
			return nil
		},
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}

	sess, err := store.Get("chat-test-transcript")
	if err != nil {
		t.Fatalf("Get adopted session: %v", err)
	}
	if sess.Metadata.WorkDir != workspace {
		t.Fatalf("WorkDir = %q, want %q", sess.Metadata.WorkDir, workspace)
	}
	if sess.Metadata.WorkspaceRoot != workspace {
		t.Fatalf("WorkspaceRoot = %q, want %q", sess.Metadata.WorkspaceRoot, workspace)
	}
}

func TestProcessSessionStartResolvesExistingSessionBySessionIDBeforeName(t *testing.T) {
	t.Setenv("CLYDE_SESSION_NAME", "stale-name")
	t.Setenv("CLYDE_SESSION_ID", "stable-uuid")

	store := session.NewFileStore(t.TempDir())
	sess := session.NewSession("renamed-session", "stable-uuid")
	if err := store.Create(sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	transcriptPath := filepath.Join(t.TempDir(), "stable-uuid.jsonl")
	input := bytes.NewBufferString(`{"session_id":"stable-uuid","source":"resume","transcript_path":"` + transcriptPath + `"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		LogRawEvent: func([]byte, string) error {
			return nil
		},
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}

	if result.SessionName != "renamed-session" {
		t.Fatalf("SessionName = %q, want renamed-session", result.SessionName)
	}
	updated, err := store.Get("renamed-session")
	if err != nil {
		t.Fatalf("Get renamed session: %v", err)
	}
	if updated.Metadata.ProviderTranscriptPath() != transcriptPath {
		t.Fatalf("ProviderTranscriptPath = %q, want %q", updated.Metadata.ProviderTranscriptPath(), transcriptPath)
	}
	if store.Exists("stale-name") {
		t.Fatal("stale-name was adopted, want existing session resolved by stable session id")
	}
}

func TestProcessSessionStartResumePrefersHookSessionIDOverStaleEnvSessionID(t *testing.T) {
	t.Setenv("CLYDE_SESSION_NAME", "stale-name")
	t.Setenv("CLYDE_SESSION_ID", "old-uuid")

	store := session.NewFileStore(t.TempDir())
	oldSession := session.NewSession("old-session", "old-uuid")
	if err := store.Create(oldSession); err != nil {
		t.Fatalf("Create old session: %v", err)
	}
	currentSession := session.NewSession("current-session", "current-uuid")
	if err := store.Create(currentSession); err != nil {
		t.Fatalf("Create current session: %v", err)
	}

	input := bytes.NewBufferString(`{"session_id":"current-uuid","source":"resume"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		LogRawEvent: func([]byte, string) error {
			return nil
		},
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}

	if result.SessionName != "current-session" {
		t.Fatalf("SessionName = %q, want current-session", result.SessionName)
	}
}

func TestProcessSessionStartCompactResolvesSessionByEnvSessionIDBeforeName(t *testing.T) {
	t.Setenv("CLYDE_SESSION_NAME", "stale-name")
	t.Setenv("CLYDE_SESSION_ID", "stable-uuid")

	store := session.NewFileStore(t.TempDir())
	sess := session.NewSession("renamed-session", "stable-uuid")
	if err := store.Create(sess); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	transcriptPath := filepath.Join(t.TempDir(), "compact-uuid.jsonl")
	input := bytes.NewBufferString(`{"session_id":"compact-uuid","source":"compact","transcript_path":"` + transcriptPath + `"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		LogRawEvent: func([]byte, string) error {
			return nil
		},
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}

	if result.SessionName != "renamed-session" {
		t.Fatalf("SessionName = %q, want renamed-session", result.SessionName)
	}
	updated, err := store.Get("renamed-session")
	if err != nil {
		t.Fatalf("Get renamed session: %v", err)
	}
	if updated.Metadata.ProviderTranscriptPath() != transcriptPath {
		t.Fatalf("ProviderTranscriptPath = %q, want %q", updated.Metadata.ProviderTranscriptPath(), transcriptPath)
	}
	if updated.Metadata.ProviderSessionID() != "compact-uuid" {
		t.Fatalf("ProviderSessionID = %q, want compact-uuid", updated.Metadata.ProviderSessionID())
	}
	previousIDs := updated.Metadata.PreviousProviderSessionIDStrings()
	if len(previousIDs) != 1 || previousIDs[0] != "stable-uuid" {
		t.Fatalf("PreviousProviderSessionIDStrings = %v, want [stable-uuid]", previousIDs)
	}
}
