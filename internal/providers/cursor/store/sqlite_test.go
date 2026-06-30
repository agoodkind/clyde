package cursorstore

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyDatabaseAllowsReadsAndRejectsWrites(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	value, found, err := ReadKVValue(context.Background(), readonly, KVTableCursorDiskKV, "composerData:composer-a")
	if err != nil {
		t.Fatalf("ReadKVValue returned error: %v", err)
	}
	if !found {
		t.Fatal("ReadKVValue found = false, want true")
	}
	if !bytes.Contains(value, []byte(`"composerId":"composer-a"`)) {
		t.Fatalf("composerData value = %s", value)
	}

	_, err = readonly.ExecContext(
		context.Background(),
		"INSERT INTO ItemTable(key, value) VALUES ('blocked', '{}')",
	)
	if err == nil {
		t.Fatal("ExecContext write returned nil error, want readonly failure")
	}
}

func TestOpenReadOnlyDatabaseRejectsNilContext(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)

	readonly, err := OpenReadOnlyDatabase(nil, dbPath)
	if err == nil {
		t.Fatal("OpenReadOnlyDatabase(nil) returned nil error, want nil-context guard")
	}
	if readonly != nil {
		t.Fatalf("OpenReadOnlyDatabase(nil) returned db %v, want nil", readonly)
	}
}

func TestTableExistsReportsPresentAndMissingTables(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	found, err := TableExists(context.Background(), readonly, "cursorDiskKV")
	if err != nil {
		t.Fatalf("TableExists returned error: %v", err)
	}
	if !found {
		t.Fatal("TableExists(cursorDiskKV) = false, want true")
	}

	missing, err := TableExists(context.Background(), readonly, "missing")
	if err != nil {
		t.Fatalf("TableExists returned error: %v", err)
	}
	if missing {
		t.Fatal("TableExists(missing) = true, want false")
	}
}

func TestReadKVValueReadsExactKeysFromKnownTables(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	itemValue, found, err := ReadKVValue(context.Background(), readonly, KVTableItemTable, "item-key")
	if err != nil {
		t.Fatalf("ReadKVValue returned error: %v", err)
	}
	if !found {
		t.Fatal("ReadKVValue item-key found = false, want true")
	}
	if !bytes.Equal(itemValue, []byte(`{"kind":"item"}`)) {
		t.Fatalf("item value = %s", itemValue)
	}

	missingValue, found, err := ReadKVValue(context.Background(), readonly, KVTableCursorDiskKV, "missing")
	if err != nil {
		t.Fatalf("ReadKVValue missing returned error: %v", err)
	}
	if found {
		t.Fatal("ReadKVValue missing found = true, want false")
	}
	if missingValue != nil {
		t.Fatalf("ReadKVValue missing value = %q, want nil", missingValue)
	}
}

func TestReadKVRowsByPrefixEnumeratesOrderedRows(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	rows, err := ReadKVRowsByPrefix(
		context.Background(),
		readonly,
		KVTableCursorDiskKV,
		"bubbleId:composer-a:",
	)
	if err != nil {
		t.Fatalf("ReadKVRowsByPrefix returned error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows len = %d, want 2", len(rows))
	}
	if rows[0].Key != "bubbleId:composer-a:bubble-1" {
		t.Fatalf("rows[0].Key = %q", rows[0].Key)
	}
	if rows[1].Key != "bubbleId:composer-a:bubble-2" {
		t.Fatalf("rows[1].Key = %q", rows[1].Key)
	}
	if !bytes.Contains(rows[0].Value, []byte(`"_v":3`)) {
		t.Fatalf("rows[0].Value = %s", rows[0].Value)
	}
}

func TestKVTableNameRejectsUnknownTables(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	_, _, err = ReadKVValue(context.Background(), readonly, KVTableName("missing"), "key")
	if err == nil {
		t.Fatal("ReadKVValue returned nil error for unknown table")
	}

	_, err = ReadKVRowsByPrefix(context.Background(), readonly, KVTableName("missing"), "key")
	if err == nil {
		t.Fatal("ReadKVRowsByPrefix returned nil error for unknown table")
	}
}

func createCursorStoreTestDatabase(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}

	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('item-key', '{"kind":"item"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:composer-a', '{"composerId":"composer-a","name":"Investigate Cursor","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"status":"none","unifiedMode":"agent","forceMode":"","fullConversationHeadersOnly":[{"bubbleId":"bubble-1","type":1},{"bubbleId":"bubble-2","type":2}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:composer-b', '{"composerId":"composer-b","name":"","createdAt":1710000000200,"lastUpdatedAt":1710000000300,"status":"none","unifiedMode":"chat","forceMode":"","fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-1', '{"_v":3,"text":"hello"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-2', '{"_v":3,"text":"world"}')`,
	}
	for _, statement := range statements {
		if _, err := writable.Exec(statement); err != nil {
			_ = writable.Close()
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	return dbPath
}
