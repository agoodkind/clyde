// Package checkpointtest writes and reads conversation fingerprint record files
// in the daemon's JSON format for tests. Tests in the vectorsearch, CLI, and
// Milvus integration packages use it.
package checkpointtest

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

const (
	recordDirMode  = 0o700
	recordFileMode = 0o600
)

// record represents one fingerprint record file in the daemon's JSON format.
// It contains a conversation ID and a fingerprint.
type record struct {
	ConversationID string `json:"conversation_id"`
	Fingerprint    string `json:"fingerprint"`
}

// WriteRecord creates directory if it does not exist and writes a record file
// named fileName with the given conversation ID and fingerprint.
// It fails the test on any error.
func WriteRecord(t *testing.T, directory string, fileName string, conversationID string, fingerprint string) {
	t.Helper()
	if err := os.MkdirAll(directory, recordDirMode); err != nil {
		t.Fatalf("create %s: %v", directory, err)
	}
	content, err := json.Marshal(record{ConversationID: conversationID, Fingerprint: fingerprint})
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	path := filepath.Join(directory, fileName)
	if err := os.WriteFile(path, content, recordFileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	slog.Debug("conversation.vectorsearch.checkpointtest.record_written",
		"concern", "conversation.semantic",
		"component", "conversation",
		"path", path,
	)
}

// Records decodes every file in directory and returns fingerprints by
// conversation ID. It fails the test if it cannot read or decode a file, or if
// two files store the same conversation ID.
func Records(t *testing.T, directory string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	records := make(map[string]string, len(entries))
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		var decoded record
		if decodeErr := json.Unmarshal(content, &decoded); decodeErr != nil {
			t.Fatalf("decode %s: %v", path, decodeErr)
		}
		if _, found := records[decoded.ConversationID]; found {
			t.Fatalf("%s stores a second record for %s", directory, decoded.ConversationID)
		}
		records[decoded.ConversationID] = decoded.Fingerprint
	}
	return records
}
