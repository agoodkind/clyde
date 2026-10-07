package codestore_test

import (
	"encoding/binary"
	"os"
	"testing"
)

const (
	tornHeaderBytes  = 2
	firstLengthByte  = 3
	firstPayloadByte = 20
)

func frameHeaderBytes(format int) int {
	if format == lengthCheckedFrameFormat {
		return 3 * frameFieldBytes
	}
	return 2 * frameFieldBytes
}

func frameEnds(t *testing.T, raw []byte, format int) []int {
	t.Helper()
	headerBytes := frameHeaderBytes(format)
	ends := make([]int, 0)
	for offset := 0; offset < len(raw); {
		end := offset + headerBytes + int(binary.LittleEndian.Uint32(raw[offset:]))
		if end > len(raw) {
			t.Fatalf("frame at offset %d ends past the row log", offset)
		}
		ends = append(ends, end)
		offset = end
	}
	return ends
}

func writeThreeRows(t *testing.T, root string, format int) {
	t.Helper()
	model := loadModel(t)
	if format == legacyFrameFormat {
		writeLegacyHeader(t, root)
	} else {
		ensured, _ := openCollection(t, root)
		ensured.Close()
	}
	store := openStore(t, root)
	upsert(t, store,
		testRow(t, model, "a", "claude:a", "first message"),
		testRow(t, model, "b", "claude:b", "second message"),
		testRow(t, model, "c", "claude:c", "third message"),
	)
	store.Close()
}

func TestStoreTruncatesTornFinalFrameBeforeZeroTail(t *testing.T) {
	cases := []struct {
		name   string
		format int
		torn   func(frame []byte, headerBytes int) []byte
	}{
		{
			name:   "length-checked frame with half its payload",
			format: lengthCheckedFrameFormat,
			torn: func(frame []byte, headerBytes int) []byte {
				return frame[:headerBytes+(len(frame)-headerBytes)/2]
			},
		},
		{
			name:   "length-checked frame with two header bytes",
			format: lengthCheckedFrameFormat,
			torn: func(frame []byte, _ int) []byte {
				return frame[:tornHeaderBytes]
			},
		},
		{
			name:   "payload-checksum frame with half its payload",
			format: legacyFrameFormat,
			torn: func(frame []byte, headerBytes int) []byte {
				return frame[:headerBytes+(len(frame)-headerBytes)/2]
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			writeThreeRows(t, root, testCase.format)
			path := rowLogPath(root)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read row log: %v", err)
			}
			ends := frameEnds(t, raw, testCase.format)
			if len(ends) != 3 {
				t.Fatalf("row log has %d frames, want 3", len(ends))
			}
			validSize := ends[1]
			torn := testCase.torn(raw[validSize:ends[2]], frameHeaderBytes(testCase.format))
			damaged := append(append(raw[:validSize:validSize], torn...), make([]byte, zeroTailBytes)...)
			if err := os.WriteFile(path, damaged, 0o600); err != nil {
				t.Fatalf("write torn row log: %v", err)
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
			if got := fileSize(t, path); got != int64(validSize) {
				t.Fatalf("row log size = %d after recovery, want %d", got, validSize)
			}
		})
	}
}

func TestPayloadChecksumStoreRefusesCorruptFirstRecordBeforeValidRecords(t *testing.T) {
	cases := []struct {
		name      string
		corruptAt int
	}{
		{name: "length", corruptAt: firstLengthByte},
		{name: "payload", corruptAt: firstPayloadByte},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			writeThreeRows(t, root, legacyFrameFormat)
			path := rowLogPath(root)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read row log: %v", err)
			}
			raw[testCase.corruptAt] ^= 0xFF
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatalf("corrupt row log: %v", err)
			}
			sizeBefore := fileSize(t, path)

			corrupted := openStore(t, root)
			defer corrupted.Close()
			if count, err := corrupted.Load(testCollection); err == nil {
				t.Fatalf("Load = %d rows, want an error for a corrupt first record", count)
			}
			if got := fileSize(t, path); got != sizeBefore {
				t.Fatalf("row log size = %d after the failed open, want %d", got, sizeBefore)
			}
		})
	}
}
