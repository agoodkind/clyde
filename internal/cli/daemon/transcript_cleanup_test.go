package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscriptFixture(t *testing.T, root, chat string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, chat+".jsonl")
	if err := os.WriteFile(file, []byte("{\"msg\":\"x\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func nullLog() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestTranscriptCleanupEvictsByMaxAge(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	writeTranscriptFixture(t, root, "young", now.Add(-1*24*time.Hour))
	writeTranscriptFixture(t, root, "stale", now.Add(-30*24*time.Hour))

	runTranscriptCleanup(context.Background(), nullLog(), root, 7*24*time.Hour, 100, func() time.Time { return now })

	if _, err := os.Stat(filepath.Join(root, "young.jsonl")); err != nil {
		t.Errorf("young should remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "stale.jsonl")); !os.IsNotExist(err) {
		t.Errorf("stale should be removed, stat err: %v", err)
	}
}

func TestTranscriptCleanupEvictsByMaxChats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	// All within max-age, but exceed max-chats=2; oldest two by mtime should be removed.
	writeTranscriptFixture(t, root, "oldest", now.Add(-3*time.Hour))
	writeTranscriptFixture(t, root, "older", now.Add(-2*time.Hour))
	writeTranscriptFixture(t, root, "newer", now.Add(-1*time.Hour))
	writeTranscriptFixture(t, root, "newest", now.Add(-30*time.Minute))

	runTranscriptCleanup(context.Background(), nullLog(), root, 7*24*time.Hour, 2, func() time.Time { return now })

	for _, want := range []string{"newer", "newest"} {
		if _, err := os.Stat(filepath.Join(root, want+".jsonl")); err != nil {
			t.Errorf("%s should remain: %v", want, err)
		}
	}
	for _, gone := range []string{"oldest", "older"} {
		if _, err := os.Stat(filepath.Join(root, gone+".jsonl")); !os.IsNotExist(err) {
			t.Errorf("%s should be removed, stat err: %v", gone, err)
		}
	}
}

func TestTranscriptCleanupHandlesMissingRoot(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	// Must not panic or error on missing root.
	runTranscriptCleanup(context.Background(), nullLog(), missing, time.Hour, 10, time.Now)
}
