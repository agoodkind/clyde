//go:build unix

package vectorsearch_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
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
	checkpointFailureChildRootEnv    = "CLYDE_CHECKPOINT_FAILURE_CHILD_ROOT"
	checkpointFailureChildMarksEnv   = "CLYDE_CHECKPOINT_FAILURE_CHILD_CHECKPOINTS"
	checkpointFailureChildTest       = "^TestUpsertAtFileSizeLimitInChildProcess$"
)

func checkpointFailureUntimedDocument(conversationID string) semsearch.SemDoc {
	doc := checkpointFailureDocument(conversationID)
	doc.TimestampUnix = vectorsearch.MissingTimestampUnix
	return doc
}

func checkpointFailureNextManifest() []semsearch.Fingerprint {
	return []semsearch.Fingerprint{
		{ConversationID: checkpointFailureBlockedChat, Value: checkpointFailureFingerprint},
		{ConversationID: checkpointFailureHealthyChat, Value: checkpointFailureNextFingerprint},
	}
}

// TestUpsertAtFileSizeLimitInChildProcess runs the second upsert with a file size limit.
// TestUpsertReturnsCheckpointFailureWithLaterFailure starts this test in a child
// process. The child lowers RLIMIT_FSIZE, and the limit ends when the child exits.
// The parent test process keeps its original limit. The go command writes its test
// log from the parent test process.
func TestUpsertAtFileSizeLimitInChildProcess(t *testing.T) {
	root := os.Getenv(checkpointFailureChildRootEnv)
	if root == "" {
		t.Skip("TestUpsertReturnsCheckpointFailureWithLaterFailure starts this test as a child process.")
	}
	dataFile, err := os.Stat(filepath.Join(root, vectorsearch.CollectionName(checkpointFailureCollectionID), checkpointFailureDataFile))
	if err != nil {
		t.Fatalf("The test could not read the size of the store data file: %v.", err)
	}
	client := openCheckpointFailureClient(t, root, os.Getenv(checkpointFailureChildMarksEnv))
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatalf("The test could not read the file size limit: %v.", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	lowered := syscall.Rlimit{Cur: uint64(dataFile.Size()), Max: original.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lowered); err != nil {
		t.Fatalf("The test could not lower the file size limit: %v.", err)
	}
	docs := []semsearch.SemDoc{checkpointFailureDocument(checkpointFailureBlockedChat), checkpointFailureDocument(checkpointFailureHealthyChat)}
	_, upsertErr := client.UpsertConversationDocuments(context.Background(), checkpointFailureCollectionID, docs, checkpointFailureNextManifest())
	if upsertErr == nil || !strings.Contains(upsertErr.Error(), checkpointFailureBlockedChat) {
		t.Fatalf("The upsert returned %v instead of a checkpoint error for %s.", upsertErr, checkpointFailureBlockedChat)
	}
	if !errors.Is(upsertErr, syscall.EFBIG) {
		t.Fatalf("The upsert error %v does not include the later file size limit error.", upsertErr)
	}
}

// TestUpsertReturnsCheckpointFailureWithLaterFailure checks that the second upsert
// returns an earlier checkpoint error together with a later file size limit error.
// A child process lowers RLIMIT_FSIZE and runs the second upsert. The limit prevents
// the store data file from growing during a row write or timestamp repair for the
// second conversation. After the child exits, SyncConversationManifest returns both
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
			blockCheckpointFailureChat(t, checkpoints)

			firstManifest := []semsearch.Fingerprint{
				{ConversationID: checkpointFailureBlockedChat, Value: checkpointFailureFingerprint},
				{ConversationID: checkpointFailureHealthyChat, Value: checkpointFailureFingerprint},
			}
			if _, err := client.UpsertConversationDocuments(ctx, checkpointFailureCollectionID, testCase.firstDocs, firstManifest); err == nil {
				t.Fatalf("The first upsert did not report the checkpoint error for %s.", checkpointFailureBlockedChat)
			}
			if err := client.Close(ctx); err != nil {
				t.Fatalf("The test could not close the local vector client: %v.", err)
			}

			testBinary, err := os.Executable()
			if err != nil {
				t.Fatalf("The test could not determine its binary path: %v.", err)
			}
			child := exec.CommandContext(t.Context(), testBinary, "-test.run="+checkpointFailureChildTest, "-test.count=1")
			child.Env = append(os.Environ(), checkpointFailureChildRootEnv+"="+root, checkpointFailureChildMarksEnv+"="+checkpoints)
			if output, err := child.CombinedOutput(); err != nil {
				t.Fatalf("The child process failed: %v. It wrote this output: %s", err, output)
			}

			restarted := openCheckpointFailureClient(t, root, checkpoints)
			t.Cleanup(func() { _ = restarted.Close(ctx) })
			needed, err := restarted.SyncConversationManifest(ctx, checkpointFailureCollectionID, checkpointFailureNextManifest())
			if err != nil {
				t.Fatalf("The client could not check the conversation manifest: %v.", err)
			}
			if !slices.Equal(needed, []string{checkpointFailureBlockedChat, checkpointFailureHealthyChat}) {
				t.Fatalf("SyncConversationManifest returned %v; it must return %s and %s.", needed, checkpointFailureBlockedChat, checkpointFailureHealthyChat)
			}
		})
	}
}
