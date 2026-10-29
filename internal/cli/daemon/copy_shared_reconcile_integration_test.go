//go:build milvusintegration

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

const (
	reconcileSourceCollectionID = "integration-reconcile-single"
	reconcileTargetCollectionID = "integration-reconcile-shared"
	reconcileConversations      = 40
	reconcileMessages           = 10
	reconcileMissingRows        = 120
	reconcileMismatchedRows     = 110
	reconcileExtraRows          = 110
	reconcileQueryLimit         = 1000
)

// reconcileDocuments returns ten distinct messages for each of forty
// conversations.
func reconcileDocuments() ([]semsearch.SemDoc, []semsearch.Fingerprint) {
	docs := make([]semsearch.SemDoc, 0, reconcileConversations*reconcileMessages)
	manifest := make([]semsearch.Fingerprint, 0, reconcileConversations)
	for conversationIndex := range reconcileConversations {
		conversationID := fmt.Sprintf("codex:reconcile-%02d", conversationIndex)
		for messageIndex := range reconcileMessages {
			text := fmt.Sprintf("Conversation %d message %d records that etcd member %d restarted.", conversationIndex, messageIndex, messageIndex)
			doc := repairDoc(conversationID, text, 1_790_000_000+int64(conversationIndex*100+messageIndex))
			doc.MessageIndex = int32(messageIndex)
			docs = append(docs, doc)
		}
		manifest = append(manifest, semsearch.Fingerprint{ConversationID: conversationID, Value: "v1"})
	}
	return docs, manifest
}

func sortedRowIDs(t *testing.T, ctx context.Context, client *milvusclient.Client, collectionName string) []string {
	t.Helper()
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(collectionName).
		WithFilter(`id != ""`).
		WithOutputFields("id").
		WithLimit(reconcileQueryLimit).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("read row IDs of %s: %v", collectionName, err)
	}
	ids := make([]string, 0, resultSet.ResultCount)
	for rowIndex := range resultSet.ResultCount {
		id, idErr := resultSet.GetColumn("id").GetAsString(rowIndex)
		if idErr != nil {
			t.Fatalf("read row ID of %s: %v", collectionName, idErr)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func idExpression(ids []string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, `"`+id+`"`)
	}
	return "id in [" + strings.Join(quoted, ", ") + "]"
}

// TestCopySharedRepairsEveryDifferenceInOneRound copies 400 rows with
// copy-shared, deletes 120 target rows, changes metadata on 110 other target
// rows, and deletes 110 source rows that remain in the target. Each class
// exceeds the 100 IDs a copy report lists. The test runs copy-shared
// --verify-only with one repair round against a throwaway Milvus and the real
// embedding endpoint. It checks that the command succeeds and the target
// stores exactly the source row IDs.
func TestCopySharedRepairsEveryDifferenceInOneRound(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	ctx := context.Background()
	source := openRepairClient(t, address, vectorsearch.LayoutSingle)
	docs, manifest := reconcileDocuments()
	if _, err := source.UpsertConversationDocuments(ctx, reconcileSourceCollectionID, docs, manifest); err != nil {
		t.Fatalf("write single layout: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	wantRecords := writeSourceRecords(t, reconcileSourceCollectionID, manifest)

	baseURL := os.Getenv(repairEmbeddingBaseURLEnv)
	if baseURL == "" {
		baseURL = repairDefaultEmbeddingBaseURL
	}
	cfg := config.NewConfigWithDefaults()
	cfg.Conversation.Semantic.Backend = config.ConversationSemanticBackendMilvus
	cfg.Conversation.Semantic.IngestionEnabled = false
	cfg.Conversation.Semantic.MilvusAddress = address
	cfg.Conversation.Semantic.MilvusDatabase = "default"
	cfg.Conversation.Semantic.EmbeddingBaseURL = baseURL
	cfg.Conversation.Semantic.EmbeddingModel = repairEmbeddingModel
	cfg.Conversation.Semantic.EmbeddingDimension = repairEmbeddingDimension
	run := func(verifyOnly bool, rounds int) string {
		t.Helper()
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		factory := &cli.Factory{
			IOStreams: &cli.IOStreams{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
			Logger:    nil,
			Build:     cli.BuildInfo{Version: "test", Commit: "", Date: ""},
			Verbose:   func() bool { return false },
			Copy:      func() bool { return false },
			Config:    func() (*config.Config, error) { return cfg, nil },
		}
		options := copySharedOptions{
			FromCollectionID: reconcileSourceCollectionID,
			ToCollectionID:   reconcileTargetCollectionID,
			VerifyOnly:       verifyOnly,
			Resume:           false,
			RepairRounds:     rounds,
		}
		if err := runCopySharedIndex(ctx, factory, options); err != nil {
			t.Fatalf("copy-shared failed: %v\nstderr:\n%s", err, stderr.String())
		}
		return stdout.String()
	}
	run(false, 1)

	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: "default"})
	if err != nil {
		t.Fatalf("connect test client: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	sourceName := vectorsearch.CollectionName(reconcileSourceCollectionID)
	targetName := vectorsearch.CollectionName(reconcileTargetCollectionID)
	ids := sortedRowIDs(t, ctx, client, targetName)
	if len(ids) != len(docs) {
		t.Fatalf("copy stores %d rows, want %d", len(ids), len(docs))
	}
	missing := ids[:reconcileMissingRows]
	mismatched := ids[reconcileMissingRows : reconcileMissingRows+reconcileMismatchedRows]
	extra := ids[reconcileMissingRows+reconcileMismatchedRows : reconcileMissingRows+reconcileMismatchedRows+reconcileExtraRows]
	if _, err := client.Delete(ctx, milvusclient.NewDeleteOption(targetName).WithExpr(idExpression(missing))); err != nil {
		t.Fatalf("delete target rows: %v", err)
	}
	edited := make([]string, 0, len(mismatched))
	for range mismatched {
		edited = append(edited, `{"edited":true}`)
	}
	update := milvusclient.NewColumnBasedInsertOption(targetName).
		WithVarcharColumn("id", mismatched).
		WithVarcharColumn("metadata", edited).
		WithPartialUpdate(true)
	if _, err := client.Upsert(ctx, update); err != nil {
		t.Fatalf("change target metadata: %v", err)
	}
	if _, err := client.Delete(ctx, milvusclient.NewDeleteOption(sourceName).WithExpr(idExpression(extra))); err != nil {
		t.Fatalf("delete source rows: %v", err)
	}
	time.Sleep(repairVisibilityWait)

	sourceRows := len(docs) - reconcileExtraRows
	output := run(true, 1)
	wantSummary := fmt.Sprintf("Verified %d source rows: the shared copy stores %d rows", sourceRows, sourceRows)
	if !strings.Contains(output, wantSummary) {
		t.Fatalf("copy-shared --verify-only printed %q; expected it to contain %q", output, wantSummary)
	}
	sourceIDs := sortedRowIDs(t, ctx, client, sourceName)
	targetIDs := sortedRowIDs(t, ctx, client, targetName)
	if !slices.Equal(sourceIDs, targetIDs) {
		t.Fatalf("target stores %d row IDs after the repair; expected the %d source row IDs", len(targetIDs), len(sourceIDs))
	}
	wantSummaryRecords := fmt.Sprintf("Copied %d conversation fingerprint records.", len(wantRecords))
	if !strings.Contains(output, wantSummaryRecords) {
		t.Fatalf("copy-shared --verify-only printed %q; expected it to contain %q", output, wantSummaryRecords)
	}
	if got := savedRecords(t, reconcileTargetCollectionID); !maps.Equal(got, wantRecords) {
		t.Fatalf("target stores %d fingerprint records; expected the %d source records %v", len(got), len(wantRecords), wantRecords)
	}
	if got := savedRecords(t, reconcileSourceCollectionID); !maps.Equal(got, wantRecords) {
		t.Fatalf("source stores %d fingerprint records after the copy; expected %d", len(got), len(wantRecords))
	}
}

// writeSourceRecords writes a fingerprint record for each manifest conversation
// under a plain numbered file name in the source collection's record directory
// under the daemon checkpoint directory. It returns fingerprints by conversation
// ID.
func writeSourceRecords(t *testing.T, collectionID string, manifest []semsearch.Fingerprint) map[string]string {
	t.Helper()
	directory := filepath.Join(daemonsvc.ConversationCheckpointDir(), vectorsearch.CollectionName(collectionID))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create %s: %v", directory, err)
	}
	records := make(map[string]string, len(manifest))
	for position, fingerprint := range manifest {
		content, err := json.Marshal(map[string]string{"conversation_id": fingerprint.ConversationID, "fingerprint": fingerprint.Value})
		if err != nil {
			t.Fatalf("encode record: %v", err)
		}
		name := fmt.Sprintf("record-%03d.json", position)
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatalf("write record: %v", err)
		}
		records[fingerprint.ConversationID] = fingerprint.Value
	}
	return records
}

// savedRecords decodes every record file in the collection's record directory
// under the daemon checkpoint directory and returns fingerprints by conversation
// ID.
func savedRecords(t *testing.T, collectionID string) map[string]string {
	t.Helper()
	directory := filepath.Join(daemonsvc.ConversationCheckpointDir(), vectorsearch.CollectionName(collectionID))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	records := make(map[string]string, len(entries))
	for _, entry := range entries {
		content, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", entry.Name(), readErr)
		}
		var record struct {
			ConversationID string `json:"conversation_id"`
			Fingerprint    string `json:"fingerprint"`
		}
		if decodeErr := json.Unmarshal(content, &record); decodeErr != nil {
			t.Fatalf("decode %s: %v", entry.Name(), decodeErr)
		}
		records[record.ConversationID] = record.Fingerprint
	}
	return records
}
