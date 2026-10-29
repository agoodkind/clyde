//go:build milvusintegration

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	repairEmbeddingBaseURLEnv     = "CLYDE_TEST_EMBEDDING_BASE_URL"
	repairDefaultEmbeddingBaseURL = "http://[::1]:5400/v1"
	repairEmbeddingKey            = "clyde-milvus-integration"
	repairEmbeddingModel          = "nvidia/NV-EmbedCode-7b-v1"
	repairEmbeddingDimension      = 4096
	repairCollectionID            = "integration-timestamp-repair"
	repairVisibilityWait          = 3 * time.Second
	repairSnapshotLimit           = 1000
	repairProjectKey              = "Users-test-Sites-repair"
	repairStaleText               = "Which flag sets the Milvus segment size?"
	repairAmbiguousConversationID = "cursor:ambiguous-0000-0000-0000-000000000000"
	repairAmbiguousText           = "The MinIO bucket retention rule keeps thirty days."
)

type repairConversation struct {
	nativeID  string
	stamp     string
	query     string
	answer    string
	wantYear  int
	wantMonth time.Month
	wantDay   int
}

func repairConversations() []repairConversation {
	return []repairConversation{
		{
			nativeID: "a8ebd9ca-59d8-486d-a43b-f37383d92cbf",
			stamp:    "Friday, May 1, 2026, 3:04 PM (UTC-7)",
			query:    "Why does the etcd compaction alarm fire on the staging cluster?",
			answer:   "The etcd keyspace grew past its quota because automatic compaction was off.",
			wantYear: 2026, wantMonth: time.May, wantDay: 1,
		},
		{
			nativeID: "b1f0c2de-0000-4000-8000-000000000002",
			stamp:    "Monday, Jun 8, 2026, 9:15 AM (UTC+0)",
			query:    "Which command rotates the WireGuard peer keys on the edge router?",
			answer:   "Generate a new key pair and replace the peer public key on both ends.",
			wantYear: 2026, wantMonth: time.June, wantDay: 8,
		},
		{
			nativeID: "c2a1d3ef-0000-4000-8000-000000000003",
			stamp:    "Tuesday, Jul 14, 2026, 6:45 PM (UTC+2)",
			query:    "Which flag sets the Milvus segment size in the standalone config?",
			answer:   "The dataCoord segment maxSize setting controls the sealed segment size.",
			wantYear: 2026, wantMonth: time.July, wantDay: 14,
		},
	}
}

func (conversationCase repairConversation) id() string {
	return "cursor:" + conversationCase.nativeID
}

func cursorTextLine(t *testing.T, role string, text string) string {
	t.Helper()
	type part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type message struct {
		Role    string `json:"role"`
		Content []part `json:"content"`
	}
	type line struct {
		Role    string  `json:"role"`
		Message message `json:"message"`
	}
	encoded, err := json.Marshal(line{Role: role, Message: message{Role: role, Content: []part{{Type: "text", Text: text}}}})
	if err != nil {
		t.Fatalf("encode transcript line: %v", err)
	}
	return string(encoded) + "\n"
}

func (conversationCase repairConversation) rawUserText() string {
	return "<timestamp>" + conversationCase.stamp + "</timestamp>\n<user_query>\n" + conversationCase.query + "\n</user_query>"
}

func writeRepairTranscripts(t *testing.T, root string) {
	t.Helper()
	for _, conversationCase := range repairConversations() {
		body := cursorTextLine(t, "user", conversationCase.rawUserText()) +
			cursorTextLine(t, "assistant", conversationCase.answer)
		path := filepath.Join(root, repairProjectKey, "agent-transcripts", conversationCase.nativeID, conversationCase.nativeID+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create transcript directory: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}
}

func openRepairClient(t *testing.T, address string, layout vectorsearch.Layout) *vectorsearch.Client {
	t.Helper()
	baseURL := os.Getenv(repairEmbeddingBaseURLEnv)
	if baseURL == "" {
		baseURL = repairDefaultEmbeddingBaseURL
	}
	var client *vectorsearch.Client
	var err error
	deadline := time.Now().Add(time.Minute)
	for {
		client, err = vectorsearch.Open(context.Background(), vectorsearch.Options{
			Backend:            vectorsearch.BackendMilvus,
			MilvusAddress:      address,
			MilvusDatabase:     "default",
			MilvusLayout:       layout,
			EmbeddingBaseURL:   baseURL,
			EmbeddingModel:     repairEmbeddingModel,
			EmbeddingDimension: repairEmbeddingDimension,
			EmbeddingAPIKey:    repairEmbeddingKey,
			EmbeddingTimeout:   2 * time.Minute,
			CheckpointDir:      t.TempDir(),
		})
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		t.Fatalf("open %s client: %v", layout, err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	return client
}

type storedRow struct {
	relativePath  string
	content       string
	metadata      string
	timestampUnix string
	otherFields   string
	vector        []float32
}

var repairSnapshotScalars = []string{"conversationId", "parentConversationId", "role", "provider", "workspaceRoot", "archived", "messageIndex", "loadRules", "splitPart", "contentHash", "embeddingModel", "startLine", "endLine", "fileExtension"}

func cellText(t *testing.T, resultColumn column.Column, rowIndex int) string {
	t.Helper()
	if resultColumn == nil {
		t.Fatal("snapshot column is missing")
	}
	isNull, err := resultColumn.IsNull(rowIndex)
	if err != nil {
		t.Fatalf("read null state of %s: %v", resultColumn.Name(), err)
	}
	if isNull {
		return "null"
	}
	value, err := resultColumn.Get(rowIndex)
	if err != nil {
		t.Fatalf("read %s: %v", resultColumn.Name(), err)
	}
	return fmt.Sprint(value)
}

func snapshotRows(t *testing.T, address string) map[string]storedRow {
	t.Helper()
	ctx := context.Background()
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: "default"})
	if err != nil {
		t.Fatalf("connect snapshot client: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	fields := append([]string{"id", "relativePath", "content", "metadata", "timestampUnix", "vector"}, repairSnapshotScalars...)
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(vectorsearch.CollectionName(repairCollectionID)).
		WithFilter(`id != ""`).
		WithLimit(repairSnapshotLimit).
		WithOutputFields(fields...).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	vectors, isFloat := resultSet.GetColumn("vector").(*column.ColumnFloatVector)
	if !isFloat {
		t.Fatal("snapshot vector column is not a float vector")
	}
	rows := make(map[string]storedRow, resultSet.ResultCount)
	for rowIndex := range resultSet.ResultCount {
		id := cellText(t, resultSet.GetColumn("id"), rowIndex)
		var others strings.Builder
		for _, name := range repairSnapshotScalars {
			others.WriteString(name + "=" + cellText(t, resultSet.GetColumn(name), rowIndex) + ";")
		}
		rows[id] = storedRow{
			relativePath:  cellText(t, resultSet.GetColumn("relativePath"), rowIndex),
			content:       cellText(t, resultSet.GetColumn("content"), rowIndex),
			metadata:      cellText(t, resultSet.GetColumn("metadata"), rowIndex),
			timestampUnix: cellText(t, resultSet.GetColumn("timestampUnix"), rowIndex),
			otherFields:   others.String(),
			vector:        vectors.Data()[rowIndex],
		}
	}
	return rows
}

func requireSameVector(t *testing.T, id string, before []float32, after []float32) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("row %s vector has %d values after the repair, want %d", id, len(after), len(before))
	}
	for position := range before {
		if math.Float32bits(before[position]) != math.Float32bits(after[position]) {
			t.Fatalf("row %s vector value %d changed from %08x to %08x", id, position, math.Float32bits(before[position]), math.Float32bits(after[position]))
		}
	}
}

func requireRowsUnchanged(t *testing.T, label string, before map[string]storedRow, after map[string]storedRow) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s: %d rows, want %d", label, len(after), len(before))
	}
	for id, row := range before {
		current, found := after[id]
		if !found {
			t.Fatalf("%s: row %s is missing", label, id)
		}
		if current.metadata != row.metadata || current.timestampUnix != row.timestampUnix || current.content != row.content || current.otherFields != row.otherFields {
			t.Fatalf("%s: row %s changed from %+v to %+v", label, id, row, current)
		}
		requireSameVector(t, id, row.vector, current.vector)
	}
}

func repairDoc(conversationID string, text string, timestampUnix int64) semsearch.SemDoc {
	return semsearch.SemDoc{
		ConversationID: conversationID, ParentConversationID: "", MessageIndex: 0, Role: "user",
		TimestampUnix: timestampUnix, Text: text, Tools: nil, Thinking: "",
		WorkspaceRoot: "", Archived: false, LoadRules: "",
	}
}

// Seed the old timestamp and one stale message to exercise matching.
func seedStoredRows(t *testing.T, client *vectorsearch.Client, index *conversation.Index, kinds conversation.ContentKindSet) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh index: %v", err)
	}
	stamped, err := index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(stamped) != len(repairConversations()) {
		t.Fatalf("index lists %d conversations, want %d", len(stamped), len(repairConversations()))
	}
	loader := repairTimestampsRun{contentKinds: kinds, index: index}
	stale := repairConversations()[2].id()
	rawUserTexts := make(map[string]string)
	for _, conversationCase := range repairConversations() {
		rawUserTexts[conversationCase.id()] = conversationCase.rawUserText()
	}
	sourceTimes := make(map[string]int64)
	docs := make([]semsearch.SemDoc, 0)
	manifest := make([]semsearch.Fingerprint, 0)
	for _, entry := range stamped {
		source, loadErr := loader.loadSource(ctx, entry.Record)
		if loadErr != nil {
			t.Fatalf("load %s: %v", entry.Record.ID, loadErr)
		}
		if len(source.Documents) != 2 {
			t.Fatalf("%s projects %d documents, want 2", entry.Record.ID, len(source.Documents))
		}
		for _, doc := range source.Documents {
			if doc.Role == "user" {
				sourceTimes[doc.ConversationID] = doc.TimestampUnix
			}
			doc.TimestampUnix = time.Time{}.Unix()
			if doc.Role == "user" {
				doc.Text = rawUserTexts[doc.ConversationID]
			}
			if doc.ConversationID == stale && doc.Role == "user" {
				doc.Text = repairStaleText
			}
			docs = append(docs, doc)
		}
		manifest = append(manifest, semsearch.Fingerprint{ConversationID: entry.Record.ID, Value: "old-parser"})
	}
	docs = append(docs, repairDoc(repairAmbiguousConversationID, repairAmbiguousText, time.Time{}.Unix()))
	manifest = append(manifest, semsearch.Fingerprint{ConversationID: repairAmbiguousConversationID, Value: "old-parser"})
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("seed stored rows: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	return sourceTimes
}

func searchDay(t *testing.T, client *vectorsearch.Client, query string, year int, month time.Month, day int) []semsearch.SemHit {
	t.Helper()
	start := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	filter := semsearch.SearchFilter{FromUnix: start.Unix(), UntilUnix: start.Add(24 * time.Hour).Unix()}
	hits, err := client.SearchConversations(context.Background(), repairCollectionID, query, 5, filter, 0)
	if err != nil {
		t.Fatalf("search %q on %s: %v", query, start.Format(time.DateOnly), err)
	}
	return hits
}

func runRepair(t *testing.T, ctx context.Context, base repairTimestampsRun, options repairTimestampsOptions) (string, error) {
	t.Helper()
	var output bytes.Buffer
	run := base
	run.options = options
	run.out = &output
	err := run.execute(ctx)
	return output.String(), err
}

func readProgress(t *testing.T, path string) timestampRepairProgress {
	t.Helper()
	progress, err := readTimestampRepairProgress(path)
	if err != nil {
		t.Fatalf("read progress: %v", err)
	}
	return progress
}

// TestRepairTimestampsRepairsCursorRowsInMilvus checks timestamp repair and
// date-filtered search against Milvus and the real embedding endpoint.
func TestRepairTimestampsRepairsCursorRowsInMilvus(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	projects := filepath.Join(home, "cursor-projects")
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projects)
	writeRepairTranscripts(t, projects)

	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutSingle)
	registry := conversation.NewRegistry()
	registry.Register(cursorparser.New())
	index := conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation)
	kinds := backfillTestContentKinds(t)
	sourceTimes := seedStoredRows(t, client, index, kinds)
	reproduced := repairConversations()[0]
	reproducedTime := time.Unix(sourceTimes[reproduced.id()], 0).UTC()
	if reproducedTime.Year() != reproduced.wantYear || reproducedTime.Month() != reproduced.wantMonth || reproducedTime.Day() != reproduced.wantDay {
		t.Fatalf("transcript reader returns %s for %s, want 2026-05-01", reproducedTime, reproduced.id())
	}
	if hits := searchDay(t, client, reproduced.query, reproduced.wantYear, reproduced.wantMonth, reproduced.wantDay); len(hits) != 0 {
		t.Fatalf("date search before the repair returned %+v, want an empty result on the source day", hits)
	}
	seeded := snapshotRows(t, address)

	progressPath := filepath.Join(home, "progress.json")
	base := repairTimestampsRun{
		options:        repairTimestampsOptions{},
		collectionID:   repairCollectionID,
		collectionName: vectorsearch.CollectionName(repairCollectionID),
		contentKinds:   kinds,
		index:          index,
		client:         client,
		out:            nil,
	}
	bounded := repairTimestampsOptions{DryRun: false, Provider: "cursor", ConversationID: "", Limit: 10, MaxRows: 100, After: "", Resume: false, ProgressPath: progressPath}

	dryRun := bounded
	dryRun.DryRun = true
	output, err := runRepair(t, ctx, base, dryRun)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(output, "read 6 rows, found 0 current, 2 changed, 0 written, 1 unmatched, 3 without a source timestamp, 0 ambiguous") {
		t.Fatalf("dry run output = %q", output)
	}
	if _, statErr := os.Stat(progressPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("dry run wrote a progress file: %v", statErr)
	}
	requireRowsUnchanged(t, "after the dry run", seeded, snapshotRows(t, address))

	firstBatch := bounded
	firstBatch.MaxRows = 1
	if _, err := runRepair(t, ctx, base, firstBatch); err != nil {
		t.Fatalf("bounded run: %v", err)
	}
	progress := readProgress(t, progressPath)
	if progress.Cursor != reproduced.id() || progress.Complete || progress.Totals.RowsWritten != 1 {
		t.Fatalf("progress after the bounded run = %+v, want cursor %s, incomplete, 1 row written", progress, reproduced.id())
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	resumed := bounded
	resumed.Resume = true
	if _, err := runRepair(t, canceled, base, resumed); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted run returned %v, want context.Canceled", err)
	}
	if interrupted := readProgress(t, progressPath); interrupted.Cursor != reproduced.id() || interrupted.Totals.RowsWritten != 1 {
		t.Fatalf("progress after the interrupted run = %+v, want the bounded run cursor", interrupted)
	}

	output, err = runRepair(t, ctx, base, resumed)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	progress = readProgress(t, progressPath)
	if !progress.Complete || progress.Totals.RowsWritten != 2 || progress.Totals.RowsChanged != 2 || progress.Cursor != repairConversations()[2].id() {
		t.Fatalf("progress after the resumed run = %+v (output %q), want complete with 2 rows written", progress, output)
	}

	ambiguous, err := client.RepairConversationTimestamps(ctx, repairCollectionID, []vectorsearch.TimestampRepairSource{{
		ConversationID: repairAmbiguousConversationID,
		Documents: []semsearch.SemDoc{
			repairDoc(repairAmbiguousConversationID, repairAmbiguousText, 1_780_000_000),
			repairDoc(repairAmbiguousConversationID, repairAmbiguousText, 1_780_000_600),
		},
	}}, false)
	if err != nil {
		t.Fatalf("ambiguous repair: %v", err)
	}
	if ambiguous.RowsAmbiguous != 1 || ambiguous.RowsWritten != 0 {
		t.Fatalf("ambiguous repair report = %+v, want 1 ambiguous row and 0 written", ambiguous)
	}
	shared := openRepairClient(t, address, vectorsearch.LayoutShared)
	if _, err := shared.RepairConversationTimestamps(ctx, repairCollectionID, nil, true); !errors.Is(err, vectorsearch.ErrTimestampRepairUnsupported) {
		t.Fatalf("shared layout repair returned %v, want ErrTimestampRepairUnsupported", err)
	}

	repaired := snapshotRows(t, address)
	if len(repaired) != len(seeded) {
		t.Fatalf("collection stores %d rows after the repair, want %d", len(repaired), len(seeded))
	}
	zero := strconv.FormatInt(time.Time{}.Unix(), 10)
	changedRows := 0
	for id, before := range seeded {
		after := repaired[id]
		if after.relativePath != before.relativePath || after.content != before.content || after.otherFields != before.otherFields {
			t.Fatalf("row %s changed a kept field: before %+v, after %+v", id, before, after)
		}
		requireSameVector(t, id, before.vector, after.vector)
		if after.timestampUnix == before.timestampUnix {
			if after.metadata != before.metadata {
				t.Fatalf("row %s changed metadata without a timestamp change: %q to %q", id, before.metadata, after.metadata)
			}
			continue
		}
		changedRows++
		want := strconv.FormatInt(sourceTimes[strings.Split(before.relativePath, "/")[1]], 10)
		if after.timestampUnix != want {
			t.Fatalf("row %s timestampUnix = %s, want %s", id, after.timestampUnix, want)
		}
		wantMetadata := strings.Replace(before.metadata, `"timestamp_unix":`+zero, `"timestamp_unix":`+want, 1)
		if after.metadata != wantMetadata {
			t.Fatalf("row %s metadata = %q, want %q", id, after.metadata, wantMetadata)
		}
	}
	if changedRows != 2 {
		t.Fatalf("repair changed %d rows, want the 2 matched user rows", changedRows)
	}

	time.Sleep(repairVisibilityWait)
	for _, conversationCase := range repairConversations()[:2] {
		hits := searchDay(t, client, conversationCase.query, conversationCase.wantYear, conversationCase.wantMonth, conversationCase.wantDay)
		if len(hits) == 0 || hits[0].ConversationID != conversationCase.id() {
			t.Fatalf("date search for %s returned %+v, want the repaired message first", conversationCase.id(), hits)
		}
		hitTime := time.Unix(hits[0].TimestampUnix, 0).UTC()
		if hitTime.Year() != conversationCase.wantYear || hitTime.Month() != conversationCase.wantMonth || hitTime.Day() != conversationCase.wantDay {
			t.Fatalf("search hit time for %s = %s, want %d-%02d-%02d", conversationCase.id(), hitTime, conversationCase.wantYear, conversationCase.wantMonth, conversationCase.wantDay)
		}
	}

	rerun, err := runRepair(t, ctx, base, bounded)
	if err != nil {
		t.Fatalf("repeat run: %v", err)
	}
	if !strings.Contains(rerun, "read 6 rows, found 2 current, 0 changed, 0 written") {
		t.Fatalf("repeat run output = %q, want every matched row current", rerun)
	}
	requireRowsUnchanged(t, "after the repeat run", repaired, snapshotRows(t, address))
	requireInvalidMetadataUnchanged(t, ctx, address, client, base, repaired)
}

func setRowMetadata(t *testing.T, ctx context.Context, address string, id string, metadata string) {
	t.Helper()
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: "default"})
	if err != nil {
		t.Fatalf("metadata client could not connect: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	option := milvusclient.NewColumnBasedInsertOption(vectorsearch.CollectionName(repairCollectionID)).
		WithVarcharColumn("id", []string{id}).
		WithVarcharColumn("metadata", []string{metadata}).
		WithPartialUpdate(true)
	if _, err := client.Upsert(ctx, option); err != nil {
		t.Fatalf("client could not set metadata for row %s: %v", id, err)
	}
}

func requireInvalidMetadataUnchanged(t *testing.T, ctx context.Context, address string, client *vectorsearch.Client, base repairTimestampsRun, repaired map[string]storedRow) {
	t.Helper()
	target := repairConversations()[1]
	targetPath := "conv/" + target.id() + "/0"
	targetID := ""
	for id, row := range repaired {
		if row.relativePath == targetPath {
			targetID = id
		}
	}
	if targetID == "" {
		t.Fatalf("collection does not contain a row at %s", targetPath)
	}
	invalid := `{"timestamp_unix":1,"timestamp_unix":2}`
	setRowMetadata(t, ctx, address, targetID, invalid)
	stamped, err := base.index.ListWithStamps(ctx)
	if err != nil {
		t.Fatalf("index could not list conversations: %v", err)
	}
	var source vectorsearch.TimestampRepairSource
	for _, entry := range stamped {
		if entry.Record.ID == target.id() {
			source, err = base.loadSource(ctx, entry.Record)
			if err != nil {
				t.Fatalf("index could not load conversation %s: %v", target.id(), err)
			}
		}
	}
	for position := range source.Documents {
		if source.Documents[position].Role == "user" {
			source.Documents[position].TimestampUnix += 60
		}
	}
	report, err := client.RepairConversationTimestamps(ctx, repairCollectionID, []vectorsearch.TimestampRepairSource{source}, false)
	if err != nil {
		t.Fatalf("timestamp repair failed with invalid metadata: %v", err)
	}
	if report.RowsInvalidMetadata != 1 || report.RowsWritten != 0 {
		t.Fatalf("timestamp repair reported %+v; want 1 invalid row and 0 rows written", report)
	}
	after := snapshotRows(t, address)[targetID]
	if after.metadata != invalid || after.timestampUnix != repaired[targetID].timestampUnix {
		t.Fatalf("row %s after timestamp repair is %+v; want metadata %q and timestampUnix %s", targetID, after, invalid, repaired[targetID].timestampUnix)
	}
	requireSameVector(t, targetID, repaired[targetID].vector, after.vector)
}
