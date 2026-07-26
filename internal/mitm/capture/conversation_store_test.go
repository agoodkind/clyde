package capture

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyRequestsSchema is the requests table as it existed before the
// conversation join columns, kept verbatim so the test opens a database that
// really lacks them.
const legacyRequestsSchema = `
CREATE TABLE requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	client TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	concern TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL DEFAULT '',
	method TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	status INTEGER NOT NULL DEFAULT 0,
	request_id TEXT NOT NULL DEFAULT '',
	upstream_request_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	trace_id TEXT NOT NULL DEFAULT '',
	req_headers TEXT NOT NULL DEFAULT '',
	resp_headers TEXT NOT NULL DEFAULT '',
	req_content_type TEXT NOT NULL DEFAULT '',
	resp_content_type TEXT NOT NULL DEFAULT '',
	req_bytes INTEGER NOT NULL DEFAULT 0,
	resp_bytes INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0
);`

// Opening a store over a database written before the join existed must add the
// columns without touching a single stored row. The capture store deletes,
// drops, and rewrites nothing.
func TestOpeningALegacyDatabaseAddsTheJoinColumnsAndKeepsEveryRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	legacy, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	if _, err := legacy.Exec(legacyRequestsSchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacy.Exec(
		`INSERT INTO requests (ts, client, host, method, path, status, request_id) VALUES (?,?,?,?,?,?,?)`,
		int64(1785082765000000000), "app.cursor", "api2.cursor.sh", "POST", "/legacy", 200, "req-legacy",
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(context.Background(), Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("Open over legacy database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })

	ref, err := store.ConversationForRequest(context.Background(), RequestLookup{RequestID: "req-legacy"}, nil)
	if err != nil {
		t.Fatalf("ConversationForRequest on the legacy row: %v", err)
	}
	if ref.Resolved() {
		t.Fatalf("legacy row resolved to %q, want no conversation", ref.ConversationID)
	}

	verifier, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	defer func() { _ = verifier.Close() }()
	var preservedPath string
	if scanErr := verifier.QueryRow(
		`SELECT path FROM requests WHERE request_id='req-legacy'`).Scan(&preservedPath); scanErr != nil {
		t.Fatalf("read preserved legacy row: %v", scanErr)
	}
	if preservedPath != "/legacy" {
		t.Fatalf("legacy row path = %q, want %q", preservedPath, "/legacy")
	}
	var rowCount int
	if scanErr := verifier.QueryRow(`SELECT count(*) FROM requests`).Scan(&rowCount); scanErr != nil {
		t.Fatalf("count rows: %v", scanErr)
	}
	if rowCount != 1 {
		t.Fatalf("requests holds %d rows, want the one legacy row", rowCount)
	}
	var indexCount int
	if scanErr := verifier.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_requests_conversation_id'`,
	).Scan(&indexCount); scanErr != nil {
		t.Fatalf("check conversation index: %v", scanErr)
	}
	if indexCount != 1 {
		t.Fatal("idx_requests_conversation_id was not created on the upgraded database")
	}
}

// Opening the same database twice must be a no-op the second time, since the
// columns and index already exist.
func TestOpeningAnUpgradedDatabaseAgainIsANoOp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	first, err := Open(context.Background(), Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(context.Background(), "test"); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	second, err := Open(context.Background(), Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { _ = second.Close(context.Background(), "test cleanup") })
}
