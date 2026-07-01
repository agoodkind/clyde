package cursorstore

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestReadComposerHeaderReadsOneComposerDataRow(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "composer-a")
	if err != nil {
		t.Fatalf("ReadComposerHeader returned error: %v", err)
	}
	if !found {
		t.Fatal("ReadComposerHeader found = false, want true")
	}
	if header.ComposerID != "composer-a" {
		t.Fatalf("ComposerID = %q, want composer-a", header.ComposerID)
	}
	if header.Name != "Investigate Cursor" {
		t.Fatalf("Name = %q, want Investigate Cursor", header.Name)
	}
	if header.Status != "none" {
		t.Fatalf("Status = %q, want none", header.Status)
	}
	if header.UnifiedMode != "agent" {
		t.Fatalf("UnifiedMode = %q, want agent", header.UnifiedMode)
	}
	if len(header.FullConversationHeadersOnly) != 2 {
		t.Fatalf("FullConversationHeadersOnly len = %d, want 2", len(header.FullConversationHeadersOnly))
	}
	if header.FullConversationHeadersOnly[1].BubbleID != "bubble-2" {
		t.Fatalf("second BubbleID = %q, want bubble-2", header.FullConversationHeadersOnly[1].BubbleID)
	}
}

func TestReadComposerHeaderReportsMissingComposer(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	header, found, err := ReadComposerHeader(context.Background(), readonly, "missing")
	if err != nil {
		t.Fatalf("ReadComposerHeader returned error: %v", err)
	}
	if found {
		t.Fatal("ReadComposerHeader found = true, want false")
	}
	if header.ComposerID != "" {
		t.Fatalf("missing header = %#v, want zero value", header)
	}
}

func TestReadComposerHeaderWrapsComposerIDOnDecodeError(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := writable.Exec(
		"INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:broken', '{')",
	); err != nil {
		_ = writable.Close()
		t.Fatalf("insert broken composer: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	_, _, err = ReadComposerHeader(context.Background(), readonly, "broken")
	if err == nil {
		t.Fatal("ReadComposerHeader returned nil error, want decode error")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error = %q, want composer id", err)
	}
}

func TestListComposerIDsReturnsComposerDataSuffixes(t *testing.T) {
	dbPath := createCursorStoreTestDatabase(t)
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	composerIDs, err := ListComposerIDs(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ListComposerIDs returned error: %v", err)
	}
	if len(composerIDs) != 2 {
		t.Fatalf("composerIDs len = %d, want 2", len(composerIDs))
	}
	if composerIDs[0] != "composer-a" {
		t.Fatalf("composerIDs[0] = %q, want composer-a", composerIDs[0])
	}
	if composerIDs[1] != "composer-b" {
		t.Fatalf("composerIDs[1] = %q, want composer-b", composerIDs[1])
	}
}
