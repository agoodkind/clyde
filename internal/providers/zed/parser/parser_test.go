package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestDiscoverReturnsThreadsDatabaseCandidate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}

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

	now := "2026-06-27T09:00:00Z"
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
		[]byte("{}"),
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
	if candidates[0].Path != dbPath {
		t.Fatalf("candidate path = %q, want %q", candidates[0].Path, dbPath)
	}
}

func TestDiscoverReturnsThreadsDatabaseCandidateForZstdPayload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}

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

	payload := []byte(`{"version":"0.3.0","title":"Thread","updated_at":"2026-06-27T09:00:00Z","messages":[]}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter returned error: %v", err)
	}
	compressed := encoder.EncodeAll(payload, nil)
	if closeErr := encoder.Close(); closeErr != nil {
		t.Fatalf("zstd encoder close: %v", closeErr)
	}

	now := "2026-06-27T09:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"",
		"",
		"Summary",
		now,
		"zstd",
		compressed,
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
	if candidates[0].Path != dbPath {
		t.Fatalf("candidate path = %q, want %q", candidates[0].Path, dbPath)
	}
}

func TestDiscoverSkipsThreadsDatabaseCandidateWhenPayloadUnreadable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}

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

	now := "2026-06-27T09:00:00Z"
	if _, err := db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"",
		"",
		"Summary",
		now,
		"zstd",
		[]byte("not-zstd"),
		now,
	); err != nil {
		t.Fatalf("insert thread row: %v", err)
	}

	candidates, err := New().Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates len = %d, want 0", len(candidates))
	}
}
