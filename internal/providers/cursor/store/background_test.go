package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestDecodeBackgroundComposerWindowMappingJSONDecodesWindows(t *testing.T) {
	mapping, err := DecodeBackgroundComposerWindowMappingJSON([]byte(`{"1":[{"bcId":"composer-a"}],"6":[],"24":[{"bcId":"composer-b"},{"bcId":"composer-c"}]}`))
	if err != nil {
		t.Fatalf("DecodeBackgroundComposerWindowMappingJSON returned error: %v", err)
	}
	if len(mapping.Windows) != 3 {
		t.Fatalf("Windows len = %d, want 3", len(mapping.Windows))
	}
	if mapping.Windows[0].WindowID != "1" {
		t.Fatalf("first WindowID = %q, want 1", mapping.Windows[0].WindowID)
	}
	if len(mapping.Windows[0].ComposerIDs) != 1 || mapping.Windows[0].ComposerIDs[0] != "composer-a" {
		t.Fatalf("first ComposerIDs = %v, want [composer-a]", mapping.Windows[0].ComposerIDs)
	}
	if len(mapping.Windows[1].ComposerIDs) != 0 {
		t.Fatalf("empty window ComposerIDs len = %d, want 0", len(mapping.Windows[1].ComposerIDs))
	}
	if len(mapping.Windows[2].ComposerIDs) != 2 {
		t.Fatalf("third ComposerIDs len = %d, want 2", len(mapping.Windows[2].ComposerIDs))
	}
	if mapping.Windows[2].ComposerIDs[1] != "composer-c" {
		t.Fatalf("third ComposerIDs[1] = %q, want composer-c", mapping.Windows[2].ComposerIDs[1])
	}
}

func TestDecodeBackgroundComposerWindowMappingJSONRejectsBareStringRefs(t *testing.T) {
	_, err := DecodeBackgroundComposerWindowMappingJSON([]byte(`{"6":["composer-a"]}`))
	if err == nil {
		t.Fatal("DecodeBackgroundComposerWindowMappingJSON returned nil error for bare-string refs, want decode error")
	}
	var typedErr CursorJSONDecodeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %T, want CursorJSONDecodeError", err)
	}
}

func TestDecodeBackgroundComposerWindowMappingJSONWrapsInvalidJSON(t *testing.T) {
	_, err := DecodeBackgroundComposerWindowMappingJSON([]byte(`{"6":`))
	if err == nil {
		t.Fatal("DecodeBackgroundComposerWindowMappingJSON returned nil error, want decode error")
	}
	var typedErr CursorJSONDecodeError
	if !errors.As(err, &typedErr) {
		t.Fatalf("error = %T, want CursorJSONDecodeError", err)
	}
}

func TestListBackgroundComposersReturnsWindowMappingsFromWindowMappingKey(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		`INSERT INTO ItemTable(key, value) VALUES ('backgroundComposer.windowBcMapping', '{"6":[{"bcId":"composer-a"}],"24":[{"bcId":"composer-b"},{"bcId":"composer-c"}]}')`,
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

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	composers, err := ListBackgroundComposers(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ListBackgroundComposers returned error: %v", err)
	}
	if len(composers) != 3 {
		t.Fatalf("composers len = %d, want 3", len(composers))
	}
	if composers[0].ComposerID != "composer-a" {
		t.Fatalf("first ComposerID = %q, want composer-a", composers[0].ComposerID)
	}
	if composers[0].WindowID != "6" {
		t.Fatalf("first WindowID = %q, want 6", composers[0].WindowID)
	}
	if composers[2].ComposerID != "composer-c" {
		t.Fatalf("third ComposerID = %q, want composer-c", composers[2].ComposerID)
	}
}

func TestListBackgroundComposersReturnsEmptyOnMalformedWindowMapping(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := writable.Exec(
		`INSERT INTO ItemTable(key, value) VALUES ('backgroundComposer.windowBcMapping', '{')`,
	); err != nil {
		_ = writable.Close()
		t.Fatalf("insert malformed background mapping: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	composers, err := ListBackgroundComposers(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ListBackgroundComposers returned error: %v", err)
	}
	if len(composers) != 0 {
		t.Fatalf("composers len = %d, want 0", len(composers))
	}
}
