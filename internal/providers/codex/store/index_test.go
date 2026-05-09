package codexstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadSessionIndexLatestNameWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_index.jsonl")
	body := `{"id":"thread-1","thread_name":"old","updated_at":"2026-05-02T17:00:00Z"}` + "\n" +
		`not-json` + "\n" +
		`{"id":"thread-2","thread_name":"other","updated_at":"2026-05-02T17:01:00Z"}` + "\n" +
		`{"id":"thread-1","thread_name":"new","updated_at":"2026-05-02T17:02:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}

	idx, err := ReadSessionIndex(path)
	if err != nil {
		t.Fatalf("ReadSessionIndex returned error: %v", err)
	}
	if got := idx.ThreadName("thread-1"); got != "new" {
		t.Fatalf("ThreadName = %q, want new", got)
	}
}

func TestAppendThreadNameWritesNormalizedLatestEntry(t *testing.T) {
	previousTime := currentTime
	currentTime = func() time.Time {
		return time.Date(2026, 5, 8, 17, 30, 0, 0, time.UTC)
	}
	t.Cleanup(func() {
		currentTime = previousTime
	})

	codexHome := t.TempDir()
	paths, err := ResolveStorePaths(codexHome, codexHome)
	if err != nil {
		t.Fatalf("ResolveStorePaths: %v", err)
	}
	if err := AppendThreadName(paths, " thread-1 ", "  My renamed thread  "); err != nil {
		t.Fatalf("AppendThreadName returned error: %v", err)
	}

	idx, err := ReadSessionIndex(paths.SessionIndexPath)
	if err != nil {
		t.Fatalf("ReadSessionIndex returned error: %v", err)
	}
	if got := idx.ThreadName("thread-1"); got != "My renamed thread" {
		t.Fatalf("ThreadName = %q, want My renamed thread", got)
	}

	content, err := os.ReadFile(paths.SessionIndexPath)
	if err != nil {
		t.Fatalf("read session index: %v", err)
	}
	wantTimestamp := `"updated_at":"2026-05-08T17:30:00Z"`
	if !strings.Contains(string(content), wantTimestamp) {
		t.Fatalf("session index missing timestamp %s: %s", wantTimestamp, content)
	}
}

func TestAppendThreadNameRejectsEmptyName(t *testing.T) {
	codexHome := t.TempDir()
	paths, err := ResolveStorePaths(codexHome, codexHome)
	if err != nil {
		t.Fatalf("ResolveStorePaths: %v", err)
	}
	if err := AppendThreadName(paths, "thread-1", "   "); err == nil {
		t.Fatal("AppendThreadName returned nil error")
	}
}
