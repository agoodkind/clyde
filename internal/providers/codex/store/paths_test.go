package codexstore

import (
	"path/filepath"
	"testing"
)

func TestResolveStorePathsUsesCodexAndSQLiteHomes(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	sqliteHome := filepath.Join(t.TempDir(), "sqlite-home")

	paths, err := ResolveStorePaths(t.Context(), codexHome, sqliteHome)
	if err != nil {
		t.Fatalf("ResolveStorePaths returned error: %v", err)
	}

	if paths.CodexHome != codexHome {
		t.Fatalf("CodexHome = %q, want %q", paths.CodexHome, codexHome)
	}
	if paths.SQLiteHome != sqliteHome {
		t.Fatalf("SQLiteHome = %q, want %q", paths.SQLiteHome, sqliteHome)
	}
	if paths.SessionsDir != filepath.Join(codexHome, "sessions") {
		t.Fatalf("SessionsDir = %q", paths.SessionsDir)
	}
	if paths.ArchivedSessionsDir != filepath.Join(codexHome, "archived_sessions") {
		t.Fatalf("ArchivedSessionsDir = %q", paths.ArchivedSessionsDir)
	}
	if paths.SessionIndexPath != filepath.Join(codexHome, "session_index.jsonl") {
		t.Fatalf("SessionIndexPath = %q", paths.SessionIndexPath)
	}
}
