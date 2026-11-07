package localbackend_test

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/daemon"
)

const (
	ingestionCheckpointCollection = "independent-checkpoint-test"
	ingestionCheckpointFirst      = "claude:checkpoint-first"
	ingestionCheckpointSecond     = "claude:checkpoint-second"
)

func ingestionCheckpointDocuments() []semsearch.SemDoc {
	return []semsearch.SemDoc{
		{ConversationID: ingestionCheckpointFirst, MessageIndex: 0, Role: "user", TimestampUnix: 1790000000, Text: "The listener reads its configured address before startup."},
		{ConversationID: ingestionCheckpointSecond, MessageIndex: 0, Role: "assistant", TimestampUnix: 1790000001, Text: "The watcher classifies configuration changes before reload."},
	}
}

func ingestionCheckpointManifest() []semsearch.Fingerprint {
	return []semsearch.Fingerprint{
		{ConversationID: ingestionCheckpointFirst, Value: "first-v1"},
		{ConversationID: ingestionCheckpointSecond, Value: "second-v1"},
	}
}

func closeIngestionCheckpointClient(t *testing.T, client *vectorsearch.Client) func() {
	t.Helper()
	closeClient := sync.OnceFunc(func() {
		if err := client.Close(context.Background()); err != nil {
			t.Errorf("close ingestion client: %v", err)
		}
	})
	t.Cleanup(closeClient)
	return closeClient
}

func openIngestionCheckpointClient(t *testing.T, settings config.ConversationSemanticConfig) (*vectorsearch.Client, func()) {
	t.Helper()
	client, err := daemon.OpenConversationIngestionClient(t.Context(), settings)
	if err != nil {
		t.Fatalf("open independent ingestion client: %v", err)
	}
	return client, closeIngestionCheckpointClient(t, client)
}

func requireIngestionCheckpointNeeded(t *testing.T, client *vectorsearch.Client, expected ...string) {
	t.Helper()
	needed, err := client.SyncConversationManifest(t.Context(), ingestionCheckpointCollection, ingestionCheckpointManifest())
	if err != nil {
		t.Fatalf("sync ingestion manifest: %v", err)
	}
	if !slices.Equal(needed, expected) {
		t.Fatalf("needed conversations = %v, want %v", needed, expected)
	}
}

func writeIngestionCheckpointDocument(t *testing.T, client *vectorsearch.Client, position int) {
	t.Helper()
	documents := ingestionCheckpointDocuments()
	if _, err := client.UpsertConversationDocuments(t.Context(), ingestionCheckpointCollection, documents[position:position+1], ingestionCheckpointManifest()); err != nil {
		t.Fatalf("write ingestion document: %v", err)
	}
}

func ingestionCheckpointFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files[path] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("read checkpoint files: %v", err)
	}
	return files
}

func TestIndependentIngestionPreservesLegacyProgress(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	settings := config.NewConfigWithDefaults().Conversation.Semantic.ForSearchBackend(config.ConversationSemanticBackendLocal)
	settings.LocalRoot = t.TempDir()
	legacy, err := daemon.OpenConversationSearchClient(t.Context(), settings)
	if err != nil {
		t.Fatalf("open legacy client: %v", err)
	}
	closeLegacy := closeIngestionCheckpointClient(t, legacy)
	writeIngestionCheckpointDocument(t, legacy, 0)
	requireIngestionCheckpointNeeded(t, legacy, ingestionCheckpointSecond)
	closeLegacy()
	legacyDirectory := filepath.Join(daemon.ConversationCheckpointDir(), vectorsearch.CollectionName(ingestionCheckpointCollection))
	legacyFiles := ingestionCheckpointFiles(t, legacyDirectory)
	indexFiles := ingestionCheckpointFiles(t, settings.LocalRoot)

	independent, closeIndependent := openIngestionCheckpointClient(t, settings)
	requireIngestionCheckpointNeeded(t, independent, ingestionCheckpointFirst, ingestionCheckpointSecond)
	writeIngestionCheckpointDocument(t, independent, 0)
	closeIndependent()
	if after := ingestionCheckpointFiles(t, settings.LocalRoot); !maps.Equal(indexFiles, after) {
		t.Fatal("recording independent progress changed the existing index files")
	}
	reopened, closeReopened := openIngestionCheckpointClient(t, settings)
	requireIngestionCheckpointNeeded(t, reopened, ingestionCheckpointSecond)
	writeIngestionCheckpointDocument(t, reopened, 1)
	requireIngestionCheckpointNeeded(t, reopened)
	closeReopened()

	legacyRestart, err := daemon.OpenConversationSearchClient(t.Context(), settings)
	if err != nil {
		t.Fatalf("reopen legacy client: %v", err)
	}
	closeIngestionCheckpointClient(t, legacyRestart)
	requireIngestionCheckpointNeeded(t, legacyRestart, ingestionCheckpointSecond)
	if after := ingestionCheckpointFiles(t, legacyDirectory); !maps.Equal(legacyFiles, after) {
		t.Fatal("independent ingestion changed legacy checkpoint files")
	}
	completed, _ := openIngestionCheckpointClient(t, settings)
	requireIngestionCheckpointNeeded(t, completed)
}

func TestIndependentIngestionRejectsInvalidBackend(t *testing.T) {
	settings := config.NewConfigWithDefaults().Conversation.Semantic
	settings.Backend = "unknown"
	client, err := daemon.OpenConversationIngestionClient(t.Context(), settings)
	if client != nil || err == nil || !strings.Contains(err.Error(), "invalid conversation ingestion backend") {
		t.Fatalf("invalid backend returned client=%v error=%v", client, err)
	}
}
