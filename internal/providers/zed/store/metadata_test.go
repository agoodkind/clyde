package zedstore

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestReadSidebarThreadsReturnsOrderedMetadataAndEmptyWhenMissing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sidebar.sqlite")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if rows, err := ReadSidebarThreads(t.Context(), db); err != nil || len(rows) != 0 {
		t.Fatalf("ReadSidebarThreads on empty db = (%d, %v), want (0, nil)", len(rows), err)
	}

	_, err = db.Exec(`
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
		) STRICT;
	`)
	if err != nil {
		t.Fatalf("create sidebar_threads: %v", err)
	}

	updatedAt := time.Date(2026, time.June, 27, 8, 30, 0, 0, time.UTC).Format(time.RFC3339)
	createdAt := time.Date(2026, time.June, 26, 8, 30, 0, 0, time.UTC).Format(time.RFC3339)
	_, err = db.Exec(
		`INSERT INTO sidebar_threads(session_id, agent_id, title, title_override, updated_at, created_at, folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"external-agent",
		"Thread title",
		"User title",
		updatedAt,
		createdAt,
		"/repo/a\n/repo/b",
		" 1, 0 ",
		1,
		"/main/a\n/main/b",
		" 1, 0 ",
	)
	if err != nil {
		t.Fatalf("insert sidebar_threads row: %v", err)
	}
	olderUpdatedAt := time.Date(2026, time.June, 26, 8, 30, 0, 0, time.UTC).Format(time.RFC3339)
	olderCreatedAt := time.Date(2026, time.June, 25, 8, 30, 0, 0, time.UTC).Format(time.RFC3339)
	_, err = db.Exec(
		`INSERT INTO sidebar_threads(session_id, agent_id, title, title_override, updated_at, created_at, folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-0",
		"",
		"Older thread",
		"",
		olderUpdatedAt,
		olderCreatedAt,
		"",
		"",
		nil,
		"",
		"",
	)
	if err != nil {
		t.Fatalf("insert nullable archived row: %v", err)
	}

	rows, err := ReadSidebarThreads(t.Context(), db)
	if err != nil {
		t.Fatalf("ReadSidebarThreads returned error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows len = %d, want 2", len(rows))
	}
	if rows[0].TitleOverride != "User title" || !rows[0].Archived {
		t.Fatalf("metadata = %#v", rows[0])
	}
	if rows[0].FolderPaths[0] != "/repo/b" || rows[0].MainWorktreePaths[0] != "/main/b" {
		t.Fatalf("ordered paths = %#v / %#v", rows[0].FolderPaths, rows[0].MainWorktreePaths)
	}
	if rows[1].SessionID != "thread-0" || rows[1].Archived {
		t.Fatalf("nullable archived metadata = %#v", rows[1])
	}
}
