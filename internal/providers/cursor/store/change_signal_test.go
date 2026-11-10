package cursorstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestChangedComposerRowsQueryUsesIntegerPrimaryKey(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-1', '{}')`,
	})
	db, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+changedComposerRowsQuery, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id int
		var parent int
		var unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(details, "\n"), "INTEGER PRIMARY KEY") {
		t.Fatalf("query plan = %q, want INTEGER PRIMARY KEY", details)
	}
}

func TestGlobalDiscoveryRetriesAfterRefreshFailure(t *testing.T) {
	dbPath := writeCursorTestDatabase(t, []string{
		`CREATE TABLE composerHeaders(composerId TEXT, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, value BLOB)`,
		`INSERT INTO composerHeaders VALUES ('composer-a', 'workspace', 1, 1, 0, 0, '{"name":"A"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:composer-a', '{"composerId":"composer-a","fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-1', '{"_v":3,"bubbleId":"bubble-1","type":1,"text":"one"}')`,
	})
	initial := ReadGlobalDiscovery(t.Context(), dbPath)
	if initial.Err != nil || initial.Metadata.Err != nil {
		t.Fatalf("initial discovery = %v, %v", initial.Err, initial.Metadata.Err)
	}
	writer, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.Exec(`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-2', '{"_v":3,"bubbleId":"bubble-2","type":2,"text":"two"}')`); err != nil {
		t.Fatal(err)
	}
	measured := ReadGlobalDiscovery(t.Context(), dbPath)
	if measured.Err != nil || measured.Metadata.Err != nil {
		t.Fatalf("measured discovery = %v, %v", measured.Err, measured.Metadata.Err)
	}
	if _, err := writer.Exec(`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-3', '{"_v":3,"bubbleId":"bubble-3","type":2,"text":"three"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`ALTER TABLE cursorDiskKV RENAME COLUMN value TO unavailable_value`); err != nil {
		t.Fatal(err)
	}
	failed := ReadGlobalDiscovery(t.Context(), dbPath)
	if failed.Err == nil && failed.Metadata.Err == nil {
		t.Fatal("injected refresh failure was not returned")
	}
	if _, err := writer.Exec(`ALTER TABLE cursorDiskKV RENAME COLUMN unavailable_value TO value`); err != nil {
		t.Fatal(err)
	}
	retried := ReadGlobalDiscovery(t.Context(), dbPath)
	if retried.Err != nil || retried.Metadata.Err != nil {
		t.Fatalf("retried discovery = %v, %v", retried.Err, retried.Metadata.Err)
	}
	if retried.Stocks["composer-a"].StoredRows != 3 {
		t.Fatalf("retried stock = %+v", retried.Stocks["composer-a"])
	}
}
