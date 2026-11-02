package vectorsearch_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	checkpointFailureCollectionID = "checkpoint-write-failure"
	checkpointFailureBlockedChat  = "codex:checkpoint-blocked"
	checkpointFailureHealthyChat  = "codex:checkpoint-healthy"
	checkpointFailureFingerprint  = "fingerprint-v1"
	checkpointFailureBlockerFile  = "blocker"
	checkpointFailureNameChars    = 32
	checkpointFailureRows         = 2
	checkpointFailureFolderMode   = 0o700

	checkpointFailureCompletedEvent = "conversation.vectorsearch.upsert_completed"
)

func checkpointFailureDocument(conversationID string) semsearch.SemDoc {
	return semsearch.SemDoc{
		ConversationID: conversationID, MessageIndex: 0, Role: "user", TimestampUnix: 1_790_000_000,
		Text: "checkpoint write failure text for " + conversationID,
	}
}

func openCheckpointFailureClient(t *testing.T, root string, checkpoints string) *vectorsearch.Client {
	t.Helper()
	client, err := vectorsearch.Open(context.Background(), vectorsearch.Options{Backend: vectorsearch.BackendLocal, LocalRoot: root, CheckpointDir: checkpoints})
	if err != nil {
		t.Fatalf("The test could not open the local vector client: %v.", err)
	}
	return client
}

// TestUpsertReportsCheckpointFailureAndContinues checks batch recovery after a checkpoint
// write fails. A nonempty directory blocks the first checkpoint rename. The test
// verifies that the second checkpoint succeeds and that a retry reuses rows and vectors.
func TestUpsertReportsCheckpointFailureAndContinues(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	client := openCheckpointFailureClient(t, root, checkpoints)
	digest := sha256.Sum256([]byte(checkpointFailureBlockedChat))
	blockerName := hex.EncodeToString(digest[:])[:checkpointFailureNameChars] + ".json"
	blockerPath := filepath.Join(checkpoints, vectorsearch.CollectionName(checkpointFailureCollectionID), blockerName)
	if err := os.MkdirAll(blockerPath, checkpointFailureFolderMode); err != nil {
		t.Fatalf("The test could not create the directory that blocks the checkpoint: %v.", err)
	}
	if err := os.WriteFile(filepath.Join(blockerPath, checkpointFailureBlockerFile), nil, 0o600); err != nil {
		t.Fatalf("The test could not create the file that keeps the checkpoint directory nonempty: %v.", err)
	}

	docs := []semsearch.SemDoc{checkpointFailureDocument(checkpointFailureBlockedChat), checkpointFailureDocument(checkpointFailureHealthyChat)}
	manifest := []semsearch.Fingerprint{
		{ConversationID: checkpointFailureBlockedChat, Value: checkpointFailureFingerprint},
		{ConversationID: checkpointFailureHealthyChat, Value: checkpointFailureFingerprint},
	}
	var logged bytes.Buffer
	previousLogger := slog.Default()
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	_, err := client.UpsertConversationDocuments(ctx, checkpointFailureCollectionID, docs, manifest)
	slog.SetDefault(previousLogger)
	log.SetOutput(previousWriter)
	log.SetFlags(previousFlags)
	if err == nil || !strings.Contains(err.Error(), checkpointFailureBlockedChat) {
		t.Fatalf("UpsertConversationDocuments returned %v; it must report a checkpoint error for %s.", err, checkpointFailureBlockedChat)
	}
	if strings.Contains(logged.String(), checkpointFailureCompletedEvent) {
		t.Fatalf("UpsertConversationDocuments logged %s after a checkpoint failed.", checkpointFailureCompletedEvent)
	}
	status := client.IngestStatus(checkpointFailureCollectionID)
	if status.LastUpsert.RowsWritten != checkpointFailureRows {
		t.Fatalf("The upsert wrote %d rows; the test requires %d.", status.LastUpsert.RowsWritten, checkpointFailureRows)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("The test could not close the local vector client: %v.", err)
	}

	// The restarted client reads the files that the first client saved.
	restarted := openCheckpointFailureClient(t, root, checkpoints)
	t.Cleanup(func() { _ = restarted.Close(ctx) })
	needed, err := restarted.SyncConversationManifest(ctx, checkpointFailureCollectionID, manifest)
	if err != nil {
		t.Fatalf("The restarted client could not check the conversation manifest: %v.", err)
	}
	if !slices.Equal(needed, []string{checkpointFailureBlockedChat}) {
		t.Fatalf("The manifest requested %v; it must request only %s.", needed, checkpointFailureBlockedChat)
	}

	if err := os.RemoveAll(blockerPath); err != nil {
		t.Fatalf("The test could not remove the directory that blocks the checkpoint: %v.", err)
	}
	retryDocs := []semsearch.SemDoc{checkpointFailureDocument(checkpointFailureBlockedChat)}
	if _, err := restarted.UpsertConversationDocuments(ctx, checkpointFailureCollectionID, retryDocs, manifest); err != nil {
		t.Fatalf("The restarted client could not retry the conversation upsert: %v.", err)
	}
	retryStatus := restarted.IngestStatus(checkpointFailureCollectionID)
	if retryStatus.LastUpsert.RowsWritten != 0 || retryStatus.LastUpsert.VectorsEmbedded != 0 {
		t.Fatalf("The retry wrote %d rows and embedded %d vectors; each count must equal 0.", retryStatus.LastUpsert.RowsWritten, retryStatus.LastUpsert.VectorsEmbedded)
	}
	needed, err = restarted.SyncConversationManifest(ctx, checkpointFailureCollectionID, manifest)
	if err != nil {
		t.Fatalf("The client could not check the conversation manifest after retrying: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("The manifest still requested conversations after the retry: %v.", needed)
	}
}
