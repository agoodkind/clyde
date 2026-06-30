package zedstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyDatabaseAllowsReadsAndRejectsWrites(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "zed.sqlite")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = writable.Close() })

	if _, err := writable.Exec("CREATE TABLE threads(id INTEGER PRIMARY KEY, title TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := writable.Exec("INSERT INTO threads(id, title) VALUES (1, 'hello')"); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	var title string
	if err := readonly.QueryRowContext(context.Background(), "SELECT title FROM threads WHERE id = 1").Scan(&title); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if title != "hello" {
		t.Fatalf("title = %q, want hello", title)
	}

	_, err = readonly.ExecContext(context.Background(), "INSERT INTO threads(id, title) VALUES (2, 'blocked')")
	if err == nil {
		t.Fatal("ExecContext write returned nil error, want readonly failure")
	}
}

func TestOpenReadOnlyDatabaseRejectsNilContext(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "zed.sqlite")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = writable.Close() })

	if _, err := writable.Exec("CREATE TABLE threads(id INTEGER PRIMARY KEY, title TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(nil, dbPath)
	if err == nil {
		t.Fatal("OpenReadOnlyDatabase(nil) returned nil error, want nil-context guard")
	}
	if readonly != nil {
		t.Fatalf("OpenReadOnlyDatabase(nil) returned db %v, want nil", readonly)
	}
}

func TestTableExistsReportsPresentAndMissingTables(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "zed.sqlite")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = writable.Close() })

	if _, err := writable.Exec("CREATE TABLE sidebar_threads(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	found, err := TableExists(context.Background(), readonly, "sidebar_threads")
	if err != nil {
		t.Fatalf("TableExists returned error: %v", err)
	}
	if !found {
		t.Fatal("TableExists(sidebar_threads) = false, want true")
	}

	missing, err := TableExists(context.Background(), readonly, "threads")
	if err != nil {
		t.Fatalf("TableExists returned error: %v", err)
	}
	if missing {
		t.Fatal("TableExists(threads) = true, want false")
	}
}
