package vectorsearch_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	checkpointSourceID = "checkpoint-source"
	checkpointTargetID = "checkpoint-target"
	checkpointFileMode = 0o600
	checkpointDirMode  = 0o700
	readOnlyDirMode    = 0o500
)

// writeSavedRecord writes a fingerprint record in the daemon's JSON format
// under <root>/<collection>/<fileName>.
func writeSavedRecord(t *testing.T, root string, collectionID string, fileName string, conversationID string, fingerprint string) {
	t.Helper()
	directory := filepath.Join(root, vectorsearch.CollectionName(collectionID))
	if err := os.MkdirAll(directory, checkpointDirMode); err != nil {
		t.Fatalf("create %s: %v", directory, err)
	}
	content, err := json.Marshal(map[string]string{"conversation_id": conversationID, "fingerprint": fingerprint})
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	path := filepath.Join(directory, fileName)
	if err := os.WriteFile(path, content, checkpointFileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// directoryBytes returns each file's content by file name.
func directoryBytes(t *testing.T, directory string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	contents := make(map[string]string, len(entries))
	for _, entry := range entries {
		content, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", entry.Name(), readErr)
		}
		contents[entry.Name()] = string(content)
	}
	return contents
}

// decodedRecords decodes every record file in directory and returns fingerprints
// by conversation ID. It fails if a file cannot be decoded or if two files
// store the same conversation ID.
func decodedRecords(t *testing.T, directory string) map[string]string {
	t.Helper()
	records := make(map[string]string)
	for name, content := range directoryBytes(t, directory) {
		var record struct {
			ConversationID string `json:"conversation_id"`
			Fingerprint    string `json:"fingerprint"`
		}
		if err := json.Unmarshal([]byte(content), &record); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		if _, found := records[record.ConversationID]; found {
			t.Fatalf("%s stores a second record for %s", directory, record.ConversationID)
		}
		records[record.ConversationID] = record.Fingerprint
	}
	return records
}

// requireSameFiles fails if a directory lost, gained, or changed a file.
func requireSameFiles(t *testing.T, stage string, before map[string]string, after map[string]string) {
	t.Helper()
	if len(after) != len(before) {
		t.Fatalf("%s: directory stores %d files, want %d", stage, len(after), len(before))
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("%s: record %s changed", stage, name)
		}
	}
}

// neededAfterRestart opens a local client to read the saved records as a
// restarted daemon would and returns the conversations the client requests.
func neededAfterRestart(t *testing.T, root string, collectionID string, manifest []semsearch.Fingerprint) []string {
	t.Helper()
	ctx := context.Background()
	client, err := vectorsearch.Open(ctx, vectorsearch.Options{Backend: vectorsearch.BackendLocal, LocalRoot: t.TempDir(), CheckpointDir: root})
	if err != nil {
		t.Fatalf("open local client: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	needed, err := client.SyncConversationManifest(ctx, collectionID, manifest)
	if err != nil {
		t.Fatalf("sync manifest of %s: %v", collectionID, err)
	}
	return needed
}

// TestCopyConversationCheckpointsCopiesSavedRecords checks that the copy
// replaces incorrect target records and removes records absent from the source.
// For both collections, it checks that a new client requests only the
// conversation with a changed fingerprint and the conversation absent from
// the source records. It also checks that the source files retain their bytes.
func TestCopyConversationCheckpointsCopiesSavedRecords(t *testing.T) {
	root := t.TempDir()
	writeSavedRecord(t, root, checkpointSourceID, "a.json", "codex:a", "fp-a")
	writeSavedRecord(t, root, checkpointSourceID, "b.json", "codex:b", "fp-b")
	writeSavedRecord(t, root, checkpointSourceID, "c.json", "codex:c", "fp-c-saved")
	writeSavedRecord(t, root, checkpointTargetID, "b.json", "codex:b", "fp-b-wrong")
	writeSavedRecord(t, root, checkpointTargetID, "gone.json", "codex:gone", "fp-gone")
	sourceDir := filepath.Join(root, vectorsearch.CollectionName(checkpointSourceID))
	targetDir := filepath.Join(root, vectorsearch.CollectionName(checkpointTargetID))
	sourceBefore := directoryBytes(t, sourceDir)

	copied, err := vectorsearch.CopyConversationCheckpoints(root, checkpointSourceID, checkpointTargetID)
	if err != nil {
		t.Fatalf("copy records: %v", err)
	}
	if copied != 3 {
		t.Fatalf("copied %d records, want 3", copied)
	}
	wantRecords := map[string]string{"codex:a": "fp-a", "codex:b": "fp-b", "codex:c": "fp-c-saved"}
	if got := decodedRecords(t, targetDir); !maps.Equal(got, wantRecords) {
		t.Fatalf("target stores records %v, want %v", got, wantRecords)
	}

	manifest := []semsearch.Fingerprint{
		{ConversationID: "codex:a", Value: "fp-a"},
		{ConversationID: "codex:b", Value: "fp-b"},
		{ConversationID: "codex:c", Value: "fp-c-current"},
		{ConversationID: "codex:gone", Value: "fp-gone"},
	}
	want := []string{"codex:c", "codex:gone"}
	if needed := neededAfterRestart(t, root, checkpointTargetID, manifest); !slices.Equal(needed, want) {
		t.Fatalf("target requests %v, want %v", needed, want)
	}
	if needed := neededAfterRestart(t, root, checkpointSourceID, manifest); !slices.Equal(needed, want) {
		t.Fatalf("source requests %v, want %v", needed, want)
	}
	requireSameFiles(t, "after the copy", sourceBefore, directoryBytes(t, sourceDir))
}

// TestCopyConversationCheckpointsFailsWhenTheTargetIsReadOnly checks that the
// copy fails when the target directory lacks write permission and that the
// source files keep their bytes.
func TestCopyConversationCheckpointsFailsWhenTheTargetIsReadOnly(t *testing.T) {
	root := t.TempDir()
	writeSavedRecord(t, root, checkpointSourceID, "a.json", "codex:a", "fp-a")
	sourceDir := filepath.Join(root, vectorsearch.CollectionName(checkpointSourceID))
	sourceBefore := directoryBytes(t, sourceDir)
	targetDir := filepath.Join(root, vectorsearch.CollectionName(checkpointTargetID))
	if err := os.MkdirAll(targetDir, readOnlyDirMode); err != nil {
		t.Fatalf("create %s: %v", targetDir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(targetDir, checkpointDirMode) })

	if _, err := vectorsearch.CopyConversationCheckpoints(root, checkpointSourceID, checkpointTargetID); err == nil {
		t.Fatal("copy into a read-only target succeeded")
	}
	requireSameFiles(t, "after the failed copy", sourceBefore, directoryBytes(t, sourceDir))
}

// TestCopyConversationCheckpointsFailsWhenTheSourceIsMissing checks that the
// copy fails without changing target file names or bytes or creating the source
// directory.
func TestCopyConversationCheckpointsFailsWhenTheSourceIsMissing(t *testing.T) {
	root := t.TempDir()
	writeSavedRecord(t, root, checkpointTargetID, "kept.json", "codex:kept", "fp-kept")
	writeSavedRecord(t, root, checkpointTargetID, "other.json", "codex:other", "fp-other")
	targetDir := filepath.Join(root, vectorsearch.CollectionName(checkpointTargetID))
	targetBefore := directoryBytes(t, targetDir)

	if _, err := vectorsearch.CopyConversationCheckpoints(root, checkpointSourceID, checkpointTargetID); err == nil {
		t.Fatal("copy from a missing source directory succeeded")
	}
	requireSameFiles(t, "after the copy from a missing source", targetBefore, directoryBytes(t, targetDir))
	if _, err := os.Stat(filepath.Join(root, vectorsearch.CollectionName(checkpointSourceID))); !os.IsNotExist(err) {
		t.Fatalf("copy from a missing source created the source directory: %v", err)
	}
}
