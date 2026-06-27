package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiscoverReturnsVirtualCandidateForNativeZedThread(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 12, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{"version":"0.3.0","title":"Thread","messages":[],"updated_at":"2026-06-27T12:00:00Z"}`)

	writeThreadsRow(t, root, "thread-1", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-1", "", "Thread title", "", updatedAt)

	candidates, err := New().Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}

	parsed, err := ParseVirtualPath(candidates[0].Path)
	if err != nil {
		t.Fatalf("ParseVirtualPath returned error: %v", err)
	}
	if parsed.Channel != "0-stable" || parsed.SessionID != "thread-1" {
		t.Fatalf("parsed = %#v", parsed)
	}
	if candidates[0].Stamp.Size != int64(len(threadJSON)) || !candidates[0].Stamp.Mtime.Equal(updatedAt) {
		t.Fatalf("stamp = %#v", candidates[0].Stamp)
	}
}

func TestDiscoverSkipsMetadataOnlyAndExternalAgentThreads(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updatedAt := time.Date(2026, time.June, 27, 12, 30, 0, 0, time.UTC)
	threadJSON := []byte(`{"version":"0.3.0","title":"Thread","messages":[],"updated_at":"2026-06-27T12:30:00Z"}`)

	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "metadata-only", "", "Only metadata", "", updatedAt)
	writeThreadsRow(t, root, "external-1", "", updatedAt, threadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "external-1", "claude", "External", "", updatedAt)

	candidates, err := New().Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates len = %d, want 0", len(candidates))
	}
}

func writeThreadsRow(t *testing.T, root, sessionID, parentID string, updatedAt time.Time, data []byte) {
	t.Helper()
	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open threads db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS threads(id TEXT PRIMARY KEY,parent_id TEXT,folder_paths TEXT,folder_paths_order TEXT,summary TEXT NOT NULL,updated_at TEXT NOT NULL,data_type TEXT NOT NULL,data BLOB NOT NULL,created_at TEXT NOT NULL) STRICT;`); err != nil {
		t.Fatalf("create threads table: %v", err)
	}
	ts := updatedAt.Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO threads(id,parent_id,folder_paths,folder_paths_order,summary,updated_at,data_type,data,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, sessionID, parentID, "/repo", "0", "Summary", ts, "json", data, ts); err != nil {
		t.Fatalf("insert threads row: %v", err)
	}
}

func writeSidebarRow(t *testing.T, dbPath, sessionID, agentID, title, titleOverride string, updatedAt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sidebar_threads(session_id TEXT PRIMARY KEY,agent_id TEXT,title TEXT NOT NULL,title_override TEXT,updated_at TEXT NOT NULL,created_at TEXT,folder_paths TEXT,folder_paths_order TEXT,archived INTEGER DEFAULT 0,main_worktree_paths TEXT,main_worktree_paths_order TEXT) STRICT;`); err != nil {
		t.Fatalf("create sidebar table: %v", err)
	}
	ts := updatedAt.Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO sidebar_threads(session_id,agent_id,title,title_override,updated_at,created_at,folder_paths,folder_paths_order,archived,main_worktree_paths,main_worktree_paths_order) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, sessionID, nullable(agentID), title, nullable(titleOverride), ts, ts, "/repo", "0", 0, "/repo", "0"); err != nil {
		t.Fatalf("insert sidebar row: %v", err)
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
