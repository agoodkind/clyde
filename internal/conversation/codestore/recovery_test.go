package codestore_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

func openCollection(t *testing.T, root string) (*codestore.Store, *staticembed.Model) {
	t.Helper()
	model, err := staticembed.Load()
	if err != nil {
		t.Fatalf("Load model: %v", err)
	}
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}
	if err := store.EnsureCollection(context.Background(), ensure); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	return store, model
}

func upsert(t *testing.T, store *codestore.Store, rows ...collection.Row) {
	t.Helper()
	if err := store.Upsert(context.Background(), testCollection, declaration(), rows); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
}

func contents(t *testing.T, store *codestore.Store) map[string]string {
	t.Helper()
	hits, err := store.Query(context.Background(), collection.QueryRequest{Collection: testCollection, Declaration: declaration(), Filter: nil, Limit: 0})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	byID := make(map[string]string, len(hits))
	for _, hit := range hits {
		byID[hit.ID] = hit.Content
	}
	return byID
}

func rowLogPath(root string) string {
	return filepath.Join(root, testCollection, "rows.log")
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

func TestStoreRefusesRowLogWithCorruptMiddleRecord(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	path := rowLogPath(root)
	upsert(t, store, testRow(t, model, "a", "claude:a", "first message"))
	corruptAt := fileSize(t, path) / 2
	upsert(t, store, testRow(t, model, "b", "claude:b", "second message"))
	upsert(t, store, testRow(t, model, "c", "claude:c", "third message"))
	store.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read row log: %v", err)
	}
	original := raw[corruptAt]
	raw[corruptAt] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("corrupt row log: %v", err)
	}
	sizeBefore := fileSize(t, path)

	corrupted, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := corrupted.Load(testCollection); err == nil {
		t.Fatal("Load succeeded on a row log with a corrupt middle record")
	}
	corrupted.Close()
	if got := fileSize(t, path); got != sizeBefore {
		t.Fatalf("row log size = %d after the failed open, want %d", got, sizeBefore)
	}

	raw[corruptAt] = original
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("restore row log: %v", err)
	}
	repaired, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repaired.Close()
	if count, err := repaired.Load(testCollection); err != nil || count != 3 {
		t.Fatalf("Load = %d, %v; want 3 rows", count, err)
	}
}

func TestStoreRefusesRowLogEndingWithUndecodableRecord(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	upsert(t, store, testRow(t, model, "a", "claude:a", "first message"))
	store.Close()

	payload := []byte{0xFF}
	frame := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
	frame = binary.LittleEndian.AppendUint32(frame, crc32.ChecksumIEEE(payload))
	frame = append(frame, payload...)
	path := rowLogPath(root)
	rowLog, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open row log: %v", err)
	}
	if _, err := rowLog.Write(frame); err != nil {
		t.Fatalf("append record: %v", err)
	}
	if err := rowLog.Close(); err != nil {
		t.Fatalf("close row log: %v", err)
	}
	sizeBefore := fileSize(t, path)

	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.Load(testCollection); err == nil {
		t.Fatal("Load succeeded on a row log ending with a checksummed record that does not decode")
	}
	if got := fileSize(t, path); got != sizeBefore {
		t.Fatalf("row log size = %d after the failed open, want %d", got, sizeBefore)
	}
}

func TestCompactedStoreReopensWithEveryRowContent(t *testing.T) {
	const (
		rowCount = 1000
		rounds   = 12
	)
	root := t.TempDir()
	store, model := openCollection(t, root)
	for round := range rounds {
		rows := make([]collection.Row, 0, rowCount)
		for index := range rowCount {
			id := fmt.Sprintf("row-%04d", index)
			rows = append(rows, testRow(t, model, id, "claude:"+id, fmt.Sprintf("round %d of %s", round, id)))
		}
		upsert(t, store, rows...)
	}
	want := contents(t, store)
	store.Close()

	if _, err := os.Stat(rowLogPath(root)); !os.IsNotExist(err) {
		t.Fatalf("generation 0 row log still exists after compaction: %v", err)
	}
	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	got := contents(t, reopened)
	if len(got) != rowCount {
		t.Fatalf("reopened store has %d rows, want %d", len(got), rowCount)
	}
	for id, content := range want {
		if got[id] != content || content != fmt.Sprintf("round %d of %s", rounds-1, id) {
			t.Fatalf("row %s content = %q, want %q", id, got[id], content)
		}
	}
}
