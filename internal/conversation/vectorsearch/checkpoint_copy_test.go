package vectorsearch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// writeSavedRecord writes one fingerprint record file in the daemon's format
// under <root>/<collection>/.
func writeSavedRecord(t *testing.T, root string, collectionID string, conversationID string, fingerprint string) {
	t.Helper()
	directory := filepath.Join(root, vectorsearch.CollectionName(collectionID))
	if err := os.MkdirAll(directory, checkpointDirMode); err != nil {
		t.Fatalf("create %s: %v", directory, err)
	}
	content, err := json.Marshal(map[string]string{"conversation_id": conversationID, "fingerprint": fingerprint})
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	sum := sha256.Sum256([]byte(conversationID))
	path := filepath.Join(directory, hex.EncodeToString(sum[:])[:32]+".json")
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

// requireSameFiles fails if a directory lost, gained, or changed a file.
func requireSameFiles(t *testing.T, stage string, before map[string]string, after map[string]string) {
	t.Helper()
	if len(after) != len(before) {
		t.Fatalf("%s: source directory stores %d files, want %d", stage, len(after), len(before))
	}
	for name, content := range before {
		if after[name] != content {
			t.Fatalf("%s: source record %s changed", stage, name)
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

// TestCopyConversationCheckpointsCopiesSavedRecords copies three source records
// into a target with one stale record and one wrong record. A new client requests
// the conversation with a different current fingerprint and the conversation
// without a source record. The test also checks that the source files keep their
// bytes.
func TestCopyConversationCheckpointsCopiesSavedRecords(t *testing.T) {
	root := t.TempDir()
	writeSavedRecord(t, root, checkpointSourceID, "codex:a", "fp-a")
	writeSavedRecord(t, root, checkpointSourceID, "codex:b", "fp-b")
	writeSavedRecord(t, root, checkpointSourceID, "codex:c", "fp-c-saved")
	writeSavedRecord(t, root, checkpointTargetID, "codex:b", "fp-b-wrong")
	writeSavedRecord(t, root, checkpointTargetID, "codex:gone", "fp-gone")
	sourceDir := filepath.Join(root, vectorsearch.CollectionName(checkpointSourceID))
	sourceBefore := directoryBytes(t, sourceDir)

	copied, err := vectorsearch.CopyConversationCheckpoints(root, checkpointSourceID, checkpointTargetID)
	if err != nil {
		t.Fatalf("copy records: %v", err)
	}
	if copied != 3 {
		t.Fatalf("copied %d records, want 3", copied)
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
	writeSavedRecord(t, root, checkpointSourceID, "codex:a", "fp-a")
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
