package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverReturnsThreadsDatabaseCandidate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}
	now := "2026-06-27T09:00:00Z"

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE threads(
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			summary TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			data_type TEXT NOT NULL,
			data BLOB NOT NULL,
			created_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	metadataPath := filepath.Join(root, "db", "0-stable", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	metadataDB, err := sql.Open("sqlite3", "file:"+metadataPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db returned error: %v", err)
	}
	t.Cleanup(func() { _ = metadataDB.Close() })
	if _, err := metadataDB.Exec(`
		CREATE TABLE sidebar_threads(
			session_id TEXT PRIMARY KEY,
			agent_id TEXT,
			title TEXT NOT NULL,
			title_override TEXT,
			updated_at TEXT NOT NULL,
			created_at TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			archived INTEGER DEFAULT 0,
			main_worktree_paths TEXT,
			main_worktree_paths_order TEXT
		) STRICT
	`); err != nil {
		t.Fatalf("create sidebar_threads table: %v", err)
	}
	if _, err := metadataDB.Exec(
		`INSERT INTO sidebar_threads(session_id, agent_id, title, title_override, updated_at, created_at, folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"Thread title",
		"",
		now,
		now,
		"/repo",
		"0",
		0,
		"/repo",
		"0",
	); err != nil {
		t.Fatalf("insert sidebar row: %v", err)
	}

	payload := []byte(`{"version":"0.3.0","title":"Thread","updated_at":"2026-06-27T09:00:00Z","messages":[]}`)
	if _, err := db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"",
		"",
		"Summary",
		now,
		"json",
		payload,
		now,
	); err != nil {
		t.Fatalf("insert thread row: %v", err)
	}

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
		t.Fatalf("parsed virtual path = %#v", parsed)
	}
}
