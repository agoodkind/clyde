package zedstore

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestReadThreadRowsReturnsOrderedRowsAndEmptyWhenMissing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "threads.sqlite")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if rows, err := ReadThreadRows(t.Context(), db); err != nil || len(rows) != 0 {
		t.Fatalf("ReadThreadRows on empty db = (%d, %v), want (0, nil)", len(rows), err)
	}

	_, err = db.Exec(`
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
		) STRICT;
	`)
	if err != nil {
		t.Fatalf("create threads: %v", err)
	}

	updatedAt := time.Date(2026, time.June, 27, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	createdAt := time.Date(2026, time.June, 26, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)
	_, err = db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"parent-1",
		"/repo/a\n/repo/b",
		"1,0",
		"Summary",
		updatedAt,
		"zstd",
		[]byte("blob"),
		createdAt,
	)
	if err != nil {
		t.Fatalf("insert threads row: %v", err)
	}

	rows, err := ReadThreadRows(t.Context(), db)
	if err != nil {
		t.Fatalf("ReadThreadRows returned error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0].ParentSessionID != "parent-1" || rows[0].DataType != DataTypeZstd {
		t.Fatalf("row = %#v", rows[0])
	}
	if rows[0].FolderPaths[0] != "/repo/b" || string(rows[0].Data) != "blob" {
		t.Fatalf("row = %#v", rows[0])
	}
}
