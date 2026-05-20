package hook

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/session"
)

func TestProcessSessionStartStartupSavesTranscriptForPrecreatedSession(t *testing.T) {
	// The daemon pre-creates the record keyed by the provider UUID before launch.
	// The hook resolves by that UUID and persists the transcript path.
	store := session.NewFileStore(t.TempDir())
	if err := store.Create(session.NewSession("chat-precreated", "precreated-uuid")); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	transcriptPath := filepath.Join(t.TempDir(), "precreated-uuid.jsonl")
	input := bytes.NewBufferString(`{"session_id":"precreated-uuid","source":"startup","transcript_path":"` + transcriptPath + `"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		LogRawEvent: func([]byte, string) error { return nil },
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}
	if result.SessionName != "chat-precreated" {
		t.Fatalf("SessionName = %q, want chat-precreated", result.SessionName)
	}
	updated, err := store.Get("chat-precreated")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if updated.Metadata.ProviderTranscriptPath() != transcriptPath {
		t.Fatalf("ProviderTranscriptPath = %q, want %q", updated.Metadata.ProviderTranscriptPath(), transcriptPath)
	}
}

func TestProcessSessionStartStartupDoesNotAdoptUnknownUUIDByName(t *testing.T) {
	// A Claude that inherited a stale CLYDE_SESSION_NAME but whose UUID matches no
	// record must not create or attach a record by name. The discovery scanner
	// adopts genuinely-new sessions instead.
	t.Setenv("CLYDE_SESSION_NAME", "ghost-name")

	store := session.NewFileStore(t.TempDir())
	input := bytes.NewBufferString(`{"session_id":"unknown-uuid","source":"startup","transcript_path":"/tmp/unknown-uuid.jsonl"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	result, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		LogRawEvent: func([]byte, string) error { return nil },
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}
	if result.SessionName != "" {
		t.Fatalf("SessionName = %q, want empty (no name adoption)", result.SessionName)
	}
	if store.Exists("ghost-name") {
		t.Fatal("ghost-name was adopted by name, want no adoption for unknown UUID")
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

func TestProcessSessionStartStartupDoesNotOverwriteTranscriptOnUUIDMismatch(t *testing.T) {
	// A second Claude inherits a leaked CLYDE_SESSION_NAME and starts fresh with
	// its own UUID. It must not overwrite the named session's transcript path,
	// because that UUID does not belong to that session.
	t.Setenv("CLYDE_SESSION_NAME", "image-1")
	t.Setenv("CLYDE_SESSION_ID", "")

	store := session.NewFileStore(t.TempDir())
	existing := session.NewSession("image-1", "real-uuid")
	originalTranscript := filepath.Join(t.TempDir(), "real-uuid.jsonl")
	existing.Metadata.SetProviderTranscriptPath(originalTranscript)
	if err := store.Create(existing); err != nil {
		t.Fatalf("Create session: %v", err)
	}

	foreignTranscript := filepath.Join(t.TempDir(), "foreign-uuid.jsonl")
	input := bytes.NewBufferString(`{"session_id":"foreign-uuid","source":"startup","transcript_path":"` + foreignTranscript + `"}`)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := ProcessSessionStart(context.Background(), store, SessionStartConfig{
		LogRawEvent: func([]byte, string) error {
			return nil
		},
	}, log, input, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("ProcessSessionStart: %v", err)
	}

	updated, err := store.Get("image-1")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if updated.Metadata.ProviderTranscriptPath() != originalTranscript {
		t.Fatalf("ProviderTranscriptPath = %q, want unchanged %q", updated.Metadata.ProviderTranscriptPath(), originalTranscript)
	}
	if updated.Metadata.ProviderSessionID() != "real-uuid" {
		t.Fatalf("ProviderSessionID = %q, want unchanged real-uuid", updated.Metadata.ProviderSessionID())
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
