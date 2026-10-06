package codestore_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	legacyFrameFormat        = 0
	lengthCheckedFrameFormat = 1
	frameFieldBytes          = 4
	zeroTailBytes            = 64
)

type legacyHeader struct {
	Declaration collection.Declaration `json:"declaration"`
	Dimension   int                    `json:"dimension"`
	Model       string                 `json:"model"`
}

func writeLegacyHeader(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, testCollection)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create collection directory: %v", err)
	}
	encoded, err := json.Marshal(legacyHeader{Declaration: declaration(), Dimension: staticembed.Dimensions, Model: staticembed.ModelName})
	if err != nil {
		t.Fatalf("encode legacy header: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "collection.json"), encoded, 0o600); err != nil {
		t.Fatalf("write legacy header: %v", err)
	}
}

func openStore(t *testing.T, root string) *codestore.Store {
	t.Helper()
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func loadModel(t *testing.T) *staticembed.Model {
	t.Helper()
	model, err := staticembed.Load()
	if err != nil {
		t.Fatalf("Load model: %v", err)
	}
	return model
}

func headerFields(t *testing.T, root string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, testCollection, "collection.json"))
	if err != nil {
		t.Fatalf("read collection header: %v", err)
	}
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("parse collection header: %v", err)
	}
	return fields
}

func countFrames(t *testing.T, path string, format int) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read row log: %v", err)
	}
	headerBytes := 2 * frameFieldBytes
	if format == lengthCheckedFrameFormat {
		headerBytes = 3 * frameFieldBytes
	}
	count := 0
	for offset := 0; offset < len(raw); count++ {
		if len(raw)-offset < headerBytes {
			t.Fatalf("frame %d at offset %d has a partial header", count, offset)
		}
		lengthBytes := raw[offset : offset+frameFieldBytes]
		if format == lengthCheckedFrameFormat && crc32.ChecksumIEEE(lengthBytes) != binary.LittleEndian.Uint32(raw[offset+frameFieldBytes:]) {
			t.Fatalf("frame %d at offset %d has a bad length checksum", count, offset)
		}
		payloadStart := offset + headerBytes
		end := payloadStart + int(binary.LittleEndian.Uint32(lengthBytes))
		if end > len(raw) {
			t.Fatalf("frame %d at offset %d ends past the row log", count, offset)
		}
		if crc32.ChecksumIEEE(raw[payloadStart:end]) != binary.LittleEndian.Uint32(raw[payloadStart-frameFieldBytes:]) {
			t.Fatalf("frame %d at offset %d has a bad payload checksum", count, offset)
		}
		offset = end
	}
	return count
}

func TestNewCollectionWritesLengthCheckedFramesAndReopens(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	upsert(t, store, testRow(t, model, "a", "claude:a", "first message"), testRow(t, model, "b", "claude:b", "second message"))
	store.Close()

	if got := string(headerFields(t, root)["frame_format"]); got != "1" {
		t.Fatalf("frame_format = %q, want 1", got)
	}
	if got := countFrames(t, rowLogPath(root), lengthCheckedFrameFormat); got != 2 {
		t.Fatalf("row log has %d length-checked frames, want 2", got)
	}
	reopened := openStore(t, root)
	defer reopened.Close()
	got := contents(t, reopened)
	if len(got) != 2 || got["a"] != "first message" || got["b"] != "second message" {
		t.Fatalf("reopened rows = %v, want a and b", got)
	}
}

func TestStoreRefusesRowLogWithCorruptFirstRecordLength(t *testing.T) {
	const lengthHighByte = 3
	root := t.TempDir()
	store, model := openCollection(t, root)
	upsert(t, store, testRow(t, model, "a", "claude:a", "first message"))
	upsert(t, store, testRow(t, model, "b", "claude:b", "second message"))
	upsert(t, store, testRow(t, model, "c", "claude:c", "third message"))
	store.Close()

	path := rowLogPath(root)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read row log: %v", err)
	}
	raw[lengthHighByte] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("corrupt row log: %v", err)
	}
	sizeBefore := fileSize(t, path)

	corrupted := openStore(t, root)
	defer corrupted.Close()
	if count, err := corrupted.Load(testCollection); err == nil {
		t.Fatalf("Load = %d rows, want an error for a corrupt first record length", count)
	}
	if got := fileSize(t, path); got != sizeBefore {
		t.Fatalf("row log size = %d after the failed open, want %d", got, sizeBefore)
	}
}

func TestStoreTruncatesZeroFilledRowLogTail(t *testing.T) {
	cases := []struct {
		name   string
		legacy bool
	}{
		{name: "length-checked frames", legacy: false},
		{name: "payload-checksum frames", legacy: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			model := loadModel(t)
			if testCase.legacy {
				writeLegacyHeader(t, root)
			} else {
				ensured, _ := openCollection(t, root)
				ensured.Close()
			}
			store := openStore(t, root)
			upsert(t, store, testRow(t, model, "a", "claude:a", "first message"), testRow(t, model, "b", "claude:b", "second message"))
			store.Close()

			path := rowLogPath(root)
			validSize := fileSize(t, path)
			rowLog, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatalf("open row log: %v", err)
			}
			if _, err := rowLog.Write(make([]byte, zeroTailBytes)); err != nil {
				t.Fatalf("append zero tail: %v", err)
			}
			if err := rowLog.Close(); err != nil {
				t.Fatalf("close row log: %v", err)
			}

			reopened := openStore(t, root)
			defer reopened.Close()
			if count, err := reopened.Load(testCollection); err != nil || count != 2 {
				t.Fatalf("Load = %d, %v; want 2 rows", count, err)
			}
			got := contents(t, reopened)
			if len(got) != 2 || got["a"] != "first message" || got["b"] != "second message" {
				t.Fatalf("reopened rows = %v, want a and b", got)
			}
			if got := fileSize(t, path); got != validSize {
				t.Fatalf("row log size = %d after recovery, want %d", got, validSize)
			}
		})
	}
}

func upsertRound(t *testing.T, store *codestore.Store, model *staticembed.Model, round int, rowCount int) {
	t.Helper()
	rows := make([]collection.Row, 0, rowCount)
	for index := range rowCount {
		id := fmt.Sprintf("row-%04d", index)
		rows = append(rows, testRow(t, model, id, "claude:"+id, fmt.Sprintf("round %d of %s", round, id)))
	}
	upsert(t, store, rows...)
}

func TestLegacyFrameCollectionAppendsAndCompactsToLengthCheckedFrames(t *testing.T) {
	const (
		rowCount = 1000
		rounds   = 12
	)
	root := t.TempDir()
	model := loadModel(t)
	writeLegacyHeader(t, root)
	store := openStore(t, root)
	upsertRound(t, store, model, 0, rowCount)
	store.Close()
	if got := countFrames(t, rowLogPath(root), legacyFrameFormat); got != rowCount {
		t.Fatalf("legacy row log has %d frames, want %d", got, rowCount)
	}

	reopened := openStore(t, root)
	if count, err := reopened.Load(testCollection); err != nil || count != rowCount {
		t.Fatalf("Load = %d, %v; want %d rows", count, err, rowCount)
	}
	for round := 1; round < rounds; round++ {
		upsertRound(t, reopened, model, round, rowCount)
	}
	upsert(t, reopened, testRow(t, model, "extra", "claude:extra", "after compaction"))
	reopened.Close()

	fields := headerFields(t, root)
	if got := string(fields["frame_format"]); got != "1" {
		t.Fatalf("frame_format = %q after compaction, want 1", got)
	}
	if got := string(fields["generation"]); got != "1" {
		t.Fatalf("generation = %q after compaction, want 1", got)
	}
	compactedLog := filepath.Join(root, testCollection, "rows.1.log")
	if got := countFrames(t, compactedLog, lengthCheckedFrameFormat); got != rowCount+1 {
		t.Fatalf("compacted row log has %d length-checked frames, want %d", got, rowCount+1)
	}

	final := openStore(t, root)
	defer final.Close()
	got := contents(t, final)
	if len(got) != rowCount+1 || got["extra"] != "after compaction" {
		t.Fatalf("reopened store has %d rows and extra = %q, want %d rows", len(got), got["extra"], rowCount+1)
	}
	for index := range rowCount {
		id := fmt.Sprintf("row-%04d", index)
		if want := fmt.Sprintf("round %d of %s", rounds-1, id); got[id] != want {
			t.Fatalf("row %s content = %q, want %q", id, got[id], want)
		}
	}
}
