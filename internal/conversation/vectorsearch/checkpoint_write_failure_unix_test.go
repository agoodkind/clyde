//go:build unix

package vectorsearch_test

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	checkpointFailureDataFile        = "data.log"
	checkpointFailureNextFingerprint = "fingerprint-v2"
)

func checkpointFailureUntimedDocument(conversationID string) semsearch.SemDoc {
	doc := checkpointFailureDocument(conversationID)
	doc.TimestampUnix = vectorsearch.MissingTimestampUnix
	return doc
}

// upsertAtFileSizeLimit sets the process file size limit, RLIMIT_FSIZE, for one upsert.
// A write that exceeds limit fails with EFBIG, the file size limit error.
func upsertAtFileSizeLimit(t *testing.T, client *vectorsearch.Client, limit uint64, docs []semsearch.SemDoc, manifest []semsearch.Fingerprint) error {
	t.Helper()
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("The test could not read the file size limit: %v.", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	lowered := syscall.Rlimit{Cur: limit, Max: original.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lowered); err != nil {
		t.Fatalf("The test could not lower the file size limit: %v.", err)
	}
	_, upsertErr := client.UpsertConversationDocuments(context.Background(), checkpointFailureCollectionID, docs, manifest)
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("The test could not restore the file size limit: %v.", err)
	}
	return upsertErr
}

// TestUpsertReturnsCheckpointFailureWithLaterFailure checks joined upsert errors.
// The first checkpoint write fails. RLIMIT_FSIZE prevents the store data file from
// growing when the upsert writes a row or repairs a timestamp for the second
// conversation. The upsert returns both errors. SyncConversationManifest returns both
// conversation IDs for another attempt.
func TestUpsertReturnsCheckpointFailureWithLaterFailure(t *testing.T) {
	cases := []struct {
		name      string
		firstDocs []semsearch.SemDoc
	}{
		{
			name:      "row write",
			firstDocs: []semsearch.SemDoc{checkpointFailureDocument(checkpointFailureBlockedChat)},
		},
		{
			name: "timestamp repair",
			firstDocs: []semsearch.SemDoc{
				checkpointFailureDocument(checkpointFailureBlockedChat),
				checkpointFailureUntimedDocument(checkpointFailureHealthyChat),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			checkpoints := t.TempDir()
			client := openCheckpointFailureClient(t, root, checkpoints)
			t.Cleanup(func() { _ = client.Close(ctx) })
			blockCheckpointFailureChat(t, checkpoints)

			firstManifest := []semsearch.Fingerprint{
				{ConversationID: checkpointFailureBlockedChat, Value: checkpointFailureFingerprint},
				{ConversationID: checkpointFailureHealthyChat, Value: checkpointFailureFingerprint},
			}
			if _, err := client.UpsertConversationDocuments(ctx, checkpointFailureCollectionID, testCase.firstDocs, firstManifest); err == nil {
				t.Fatalf("The first upsert did not report the checkpoint error for %s.", checkpointFailureBlockedChat)
			}
			dataFile, err := os.Stat(filepath.Join(root, vectorsearch.CollectionName(checkpointFailureCollectionID), checkpointFailureDataFile))
			if err != nil {
				t.Fatalf("The test could not read the size of the store data file: %v.", err)
			}

			docs := []semsearch.SemDoc{checkpointFailureDocument(checkpointFailureBlockedChat), checkpointFailureDocument(checkpointFailureHealthyChat)}
			manifest := []semsearch.Fingerprint{
				{ConversationID: checkpointFailureBlockedChat, Value: checkpointFailureFingerprint},
				{ConversationID: checkpointFailureHealthyChat, Value: checkpointFailureNextFingerprint},
			}
			upsertErr := upsertAtFileSizeLimit(t, client, uint64(dataFile.Size()), docs, manifest)
			if upsertErr == nil || !strings.Contains(upsertErr.Error(), checkpointFailureBlockedChat) {
				t.Fatalf("The upsert returned %v instead of a checkpoint error for %s.", upsertErr, checkpointFailureBlockedChat)
			}
			if !errors.Is(upsertErr, syscall.EFBIG) {
				t.Fatalf("The upsert error %v does not include the later file size limit error.", upsertErr)
			}

			needed, err := client.SyncConversationManifest(ctx, checkpointFailureCollectionID, manifest)
			if err != nil {
				t.Fatalf("The client could not check the conversation manifest: %v.", err)
			}
			if !slices.Equal(needed, []string{checkpointFailureBlockedChat, checkpointFailureHealthyChat}) {
				t.Fatalf("SyncConversationManifest returned %v; it must return %s and %s.", needed, checkpointFailureBlockedChat, checkpointFailureHealthyChat)
			}
		})
	}
}
