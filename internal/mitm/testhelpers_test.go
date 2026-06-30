package mitm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// mustWriteLines writes each record as one JSON line to path, creating
// parent directories as needed. Shared across the snapshot, drift, and
// baseline tests that build JSONL capture transcripts.
func mustWriteLines(t *testing.T, path string, records []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	for _, rec := range records {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// containsString reports whether needle is present in haystack.
func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
