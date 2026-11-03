//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const (
	splitRecoveryCollectionID   = "integration-split-recovery"
	splitRecoveryConversationID = "codex:split-recovery"
	// splitRecoveryWriteBatch is the row count of one SharedStore occurrence write.
	splitRecoveryWriteBatch        = 256
	splitRecoveryLongBytes         = 24000
	splitRecoveryTimestamp         = int64(1_790_003_000)
	splitRecoveryWorkspace         = "/work/split-recovery"
	splitRecoveryFingerprint       = "v1"
	splitRecoveryLongPrefix        = "long message"
	splitRecoveryFinalMessage      = "final message after the split message"
	splitRecoveryConversation      = "conversationId"
	splitRecoveryWorkspaceColumn   = "workspaceRoot"
	splitRecoveryMinimumPieces     = 2
	splitRecoveryEdit              = " edited after the interruption"
	splitRecoveryAppended          = "message appended after the interruption"
	splitRecoveryEditedPrint       = "v2-edited"
	splitRecoveryRestoredPrint     = "v3-restored"
	splitRecoveryAppendedPrint     = "v4-appended"
	splitRecoveryWhitespaceVectors = 2
	splitRecoveryTailBytes         = 40
	splitRecoveryOtherMessage      = 9
	splitRecoveryMovedPrint        = "v5-moved"
	splitRecoveryMovedRoot         = "/work/split-recovery-moved"
	splitRecoveryOtherChat         = "codex:split-recovery-other"
	splitRecoveryOtherPrint        = "other-v1"
	splitRecoveryEditedTail        = "XXXXX"
	splitRecoveryWaitLimit         = 5 * time.Minute
	splitRecoveryPollDelay         = 50 * time.Millisecond
	writeRecordFolder              = ".in-progress"
	splitRecoveryNameChars         = 32
	splitRecoveryFolderMode        = 0o700
	splitRecoveryReadOnlyFolder    = 0o500
	splitRecoveryNoAccessFolder    = 0o000
	splitRecoveryProofModel        = "model-a"
	splitRecoveryOtherProofModel   = "model-b"
	// splitRecoveryStoredProof is the expected family proof in the first encoding. Software
	// updates must support stored records.
	splitRecoveryStoredProof = "98ae785ecbea6fcde7f0da4667247f37acec8c0bee4a64aa636756c9b8ef7ca1"
)

type splitRecoveryRow struct {
	content       string
	relativePath  string
	vectorKey     string
	metadata      string
	workspaceRoot string
}

type splitRecoveryFixture struct {
	t              *testing.T
	address        string
	checkpointDir  string
	reader         *milvusclient.Client
	docs           []semsearch.SemDoc
	manifest       []semsearch.Fingerprint
	collectionName string
}

func splitRecoveryDocument(position int, text string) semsearch.SemDoc {
	return semsearch.SemDoc{
		ConversationID: splitRecoveryConversationID, ParentConversationID: "", MessageIndex: int32(position), Role: "user",
		TimestampUnix: splitRecoveryTimestamp + int64(position), Text: text, Tools: nil, Thinking: "",
		WorkspaceRoot: splitRecoveryWorkspace, Archived: false, LoadRules: "",
	}
}

func splitRecoveryLongText() string {
	var builder strings.Builder
	builder.WriteString(splitRecoveryLongPrefix)
	for position := 0; builder.Len() < splitRecoveryLongBytes; position++ {
		fmt.Fprintf(&builder, " harbor survey word %05d", position)
	}
	return builder.String()
}

func splitRecoveryDocuments() []semsearch.SemDoc {
	docs := make([]semsearch.SemDoc, 0, splitRecoveryWriteBatch+1)
	for position := range splitRecoveryWriteBatch - 1 {
		docs = append(docs, splitRecoveryDocument(position, fmt.Sprintf("short message %04d about a harbor survey", position)))
	}
	docs = append(docs, splitRecoveryDocument(splitRecoveryWriteBatch-1, splitRecoveryLongText()))
	return append(docs, splitRecoveryDocument(splitRecoveryWriteBatch, splitRecoveryFinalMessage))
}

func (f *splitRecoveryFixture) rows() map[string]splitRecoveryRow {
	f.t.Helper()
	result, err := f.reader.Query(context.Background(), milvusclient.NewQueryOption(f.collectionName).
		WithFilter(fmt.Sprintf("%s == %q", splitRecoveryConversation, splitRecoveryConversationID)).
		WithOutputFields(milvusstore.IDField, milvusstore.ContentField, milvusstore.RelativePathField, milvusstore.VectorKeyField, milvusstore.MetadataField, splitRecoveryWorkspaceColumn).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		f.t.Fatalf("read stored rows: %v", err)
	}
	rows := make(map[string]splitRecoveryRow, result.ResultCount)
	for position := range result.ResultCount {
		id, idErr := result.GetColumn(milvusstore.IDField).GetAsString(position)
		content, contentErr := result.GetColumn(milvusstore.ContentField).GetAsString(position)
		path, pathErr := result.GetColumn(milvusstore.RelativePathField).GetAsString(position)
		key, keyErr := result.GetColumn(milvusstore.VectorKeyField).GetAsString(position)
		metadata, metadataErr := result.GetColumn(milvusstore.MetadataField).GetAsString(position)
		workspaceRoot, workspaceErr := result.GetColumn(splitRecoveryWorkspaceColumn).GetAsString(position)
		if err := errors.Join(idErr, contentErr, pathErr, keyErr, metadataErr, workspaceErr); err != nil {
			f.t.Fatalf("The test could not decode stored row %d: %v.", position, err)
		}
		rows[id] = splitRecoveryRow{content: content, relativePath: path, vectorKey: key, metadata: metadata, workspaceRoot: workspaceRoot}
	}
	return rows
}

// vectorBits returns each row's stored vector as exact floating-point bits.
func (f *splitRecoveryFixture) vectorBits(rows map[string]splitRecoveryRow) map[string][]uint32 {
	f.t.Helper()
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.vectorKey)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	result, err := f.reader.Query(context.Background(), milvusclient.NewQueryOption(milvusstore.VectorCollectionName(f.collectionName)).
		WithIDs(column.NewColumnVarChar(milvusstore.VectorKeyField, keys)).
		WithOutputFields(milvusstore.VectorKeyField, milvusstore.DenseVectorField).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		f.t.Fatalf("read stored vectors: %v", err)
	}
	bits := make(map[string][]uint32, result.ResultCount)
	for position := range result.ResultCount {
		key, keyErr := result.GetColumn(milvusstore.VectorKeyField).GetAsString(position)
		vector, vectorErr := milvusstore.VectorAt(result.GetColumn(milvusstore.DenseVectorField), position)
		if keyErr != nil || vectorErr != nil {
			f.t.Fatalf("decode stored vector %d: %v %v", position, keyErr, vectorErr)
		}
		rowBits := make([]uint32, 0, len(vector))
		for _, value := range vector {
			rowBits = append(rowBits, math.Float32bits(value))
		}
		bits[key] = rowBits
	}
	if len(bits) != len(keys) {
		f.t.Fatalf("the vector collection returned %d of %d vectors", len(bits), len(keys))
	}
	return bits
}

// stopWrite preserves vector rows and the first 256 occurrences. It removes later
// occurrences and the conversation's checkpoint. It returns the IDs of the long
// message's removed pieces.
func (f *splitRecoveryFixture) stopWrite(complete map[string]splitRecoveryRow) []string {
	f.t.Helper()
	longPath := ""
	for _, row := range complete {
		if strings.HasPrefix(row.content, splitRecoveryLongPrefix) {
			longPath = row.relativePath
		}
	}
	if longPath == "" {
		f.t.Fatal("the stored rows do not include the first piece of the long message")
	}
	laterPieces := make([]string, 0)
	removed := make([]string, 0)
	for id, row := range complete {
		isLaterPiece := row.relativePath == longPath && !strings.HasPrefix(row.content, splitRecoveryLongPrefix)
		if isLaterPiece {
			laterPieces = append(laterPieces, id)
		}
		if isLaterPiece || row.content == splitRecoveryFinalMessage {
			removed = append(removed, fmt.Sprintf("%q", id))
		}
	}
	if len(laterPieces)+1 < splitRecoveryMinimumPieces {
		f.t.Fatalf("The store contains %d pieces of the long message; the test expected at least %d.", len(laterPieces)+1, splitRecoveryMinimumPieces)
	}
	// An earlier case can store a piece of edited text that the complete write lacks.
	for id := range f.rows() {
		if _, known := complete[id]; !known {
			removed = append(removed, fmt.Sprintf("%q", id))
		}
	}
	filter := fmt.Sprintf("%s in [%s]", milvusstore.IDField, strings.Join(removed, ","))
	if _, err := f.reader.Delete(context.Background(), milvusclient.NewDeleteOption(f.collectionName).WithExpr(filter)); err != nil {
		f.t.Fatalf("remove the later occurrences: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.checkpointDir, f.collectionName)); err != nil {
		f.t.Fatalf("remove the test checkpoint: %v", err)
	}
	if count := len(f.rows()); count != splitRecoveryWriteBatch {
		f.t.Fatalf("The stopped write stored %d rows; the test expected %d.", count, splitRecoveryWriteBatch)
	}
	return laterPieces
}

// retry runs normal ingestion in a new client, as a restarted daemon does.
func (f *splitRecoveryFixture) retry(docs []semsearch.SemDoc, fingerprint string) {
	f.t.Helper()
	ctx := context.Background()
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: fingerprint}}
	client := openClientWithCheckpoints(f.t, f.address, vectorsearch.LayoutShared, f.checkpointDir)
	needed, err := client.SyncConversationManifest(ctx, splitRecoveryCollectionID, manifest)
	if err != nil {
		f.t.Fatalf("list needed conversations: %v", err)
	}
	if len(needed) != 1 {
		f.t.Fatalf("The manifest lists conversations %v for ingestion; the test expected the test conversation.", needed)
	}
	if _, err := client.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, docs, manifest); err != nil {
		f.t.Fatalf("run ingestion again: %v", err)
	}
	needed, err = client.SyncConversationManifest(ctx, splitRecoveryCollectionID, manifest)
	if err != nil {
		f.t.Fatalf("list needed conversations after ingestion: %v", err)
	}
	if len(needed) != 0 {
		f.t.Fatalf("The manifest still lists conversations %v after ingestion.", needed)
	}
}

func (f *splitRecoveryFixture) markStopped() {
	f.t.Helper()
	if err := vectorsearch.WriteStoppedRecordForTest(f.checkpointDir, splitRecoveryCollectionID, splitRecoveryConversationID, splitRecoveryFingerprint, integrationModel, f.docs); err != nil {
		f.t.Fatalf("write the stopped write record: %v", err)
	}
}

func (f *splitRecoveryFixture) requireComplete(rows map[string]splitRecoveryRow, complete map[string]splitRecoveryRow) {
	f.t.Helper()
	for id, want := range complete {
		got, found := rows[id]
		if !found || got.content != want.content || got.relativePath != want.relativePath {
			f.t.Fatalf("The retry stored row %s as %+v; the test expected %+v.", id, got, want)
		}
	}
}

func (f *splitRecoveryFixture) requireLaterPiecesAbsent(rows map[string]splitRecoveryRow, laterPieces []string) {
	f.t.Helper()
	for _, id := range laterPieces {
		if _, found := rows[id]; found {
			f.t.Fatalf("ingestion wrote later piece %s without evidence for this text", id)
		}
	}
	if len(rows) != splitRecoveryWriteBatch+1 {
		f.t.Fatalf("Ingestion stored %d rows; the test expected %d.", len(rows), splitRecoveryWriteBatch+1)
	}
}

// requireKept fails if the retry changes any previously stored row or vector.
func (f *splitRecoveryFixture) requireKept(kept map[string]splitRecoveryRow, keptBits map[string][]uint32) map[string]splitRecoveryRow {
	f.t.Helper()
	current := f.rows()
	for id, want := range kept {
		if got, found := current[id]; !found || got != want {
			f.t.Fatalf("Ingestion changed stored row %s to %+v; the test expected %+v.", id, got, want)
		}
	}
	if got := f.vectorBits(kept); !maps.EqualFunc(got, keptBits, slices.Equal[[]uint32]) {
		f.t.Fatalf("ingestion changed the bytes of a stored vector")
	}
	return current
}

// TestSharedLayoutRetryWritesPiecesAfterStoppedOccurrenceWrite reproduces an upsert
// interruption between occurrence writes. The first write ends with the first piece
// of a split message. Each case runs normal ingestion and compares stored rows and
// vectors with their values before ingestion.
func TestSharedLayoutRetryWritesPiecesAfterStoppedOccurrenceWrite(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	ctx := context.Background()
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("connect to Milvus: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close(context.Background()) })
	fixture := &splitRecoveryFixture{
		t: t, address: address, checkpointDir: t.TempDir(), reader: reader, docs: splitRecoveryDocuments(),
		manifest:       []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}},
		collectionName: vectorsearch.CollectionName(splitRecoveryCollectionID),
	}
	writer := openClientWithCheckpoints(t, address, vectorsearch.LayoutShared, fixture.checkpointDir)
	if _, err := writer.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, fixture.docs, fixture.manifest); err != nil {
		t.Fatalf("write the complete conversation: %v", err)
	}
	complete := fixture.rows()

	// The record allows ingestion to write the later pieces.
	laterPieces := fixture.stopWrite(complete)
	stopped := fixture.rows()
	stoppedBits := fixture.vectorBits(stopped)
	fixture.markStopped()
	fixture.retry(fixture.docs, splitRecoveryFingerprint)
	recovered := fixture.requireKept(stopped, stoppedBits)
	if !slices.Equal(sortedSplitRecoveryIDs(recovered), sortedSplitRecoveryIDs(complete)) {
		t.Fatalf("The retry stored IDs %v; the test expected %v.", sortedSplitRecoveryIDs(recovered), sortedSplitRecoveryIDs(complete))
	}
	fixture.requireComplete(recovered, complete)

	// Without the record, these rows can represent a complete older version.
	fixture.stopWrite(complete)
	fixture.retry(fixture.docs, splitRecoveryFingerprint)
	fixture.requireLaterPiecesAbsent(fixture.requireKept(stopped, stoppedBits), laterPieces)

	// The stopped write record allows ingestion to store the missing pieces of the edited
	// message.
	fixture.stopWrite(complete)
	fixture.markStopped()
	edited := slices.Clone(fixture.docs)
	edited[splitRecoveryWriteBatch-1].Text += splitRecoveryEdit
	fixture.retry(edited, splitRecoveryEditedPrint)
	withEdit := fixture.requireKept(stopped, stoppedBits)
	if !slices.ContainsFunc(slices.Collect(maps.Values(withEdit)), func(row splitRecoveryRow) bool {
		return strings.HasSuffix(row.content, splitRecoveryEdit)
	}) {
		t.Fatal("The stored rows do not include the end of the edited message.")
	}
	withEditBits := fixture.vectorBits(withEdit)

	// The store retains the changed family's record and uses it when the earlier text
	// returns. The edited rows and their exact vector bytes remain.
	fixture.retry(fixture.docs, splitRecoveryRestoredPrint)
	fixture.requireComplete(fixture.requireKept(withEdit, withEditBits), complete)

	// Ingestion uses the stopped write record after a metadata change and stores the later
	// pieces.
	fixture.stopWrite(complete)
	fixture.markStopped()
	moved := slices.Clone(fixture.docs)
	moved[splitRecoveryWriteBatch-1].WorkspaceRoot = splitRecoveryMovedRoot
	fixture.retry(moved, splitRecoveryMovedPrint)
	withMove := fixture.requireKept(stopped, stoppedBits)
	fixture.requireComplete(withMove, complete)
	movedPieces := 0
	for _, id := range laterPieces {
		if withMove[id].workspaceRoot == splitRecoveryMovedRoot {
			movedPieces++
		}
	}
	if movedPieces != len(laterPieces) {
		t.Fatalf("%d of %d later pieces store the moved workspace root.", movedPieces, len(laterPieces))
	}

	// Ingestion still recovers the unchanged family when the source fingerprint changes.
	fixture.stopWrite(complete)
	fixture.markStopped()
	appended := append(slices.Clone(fixture.docs), splitRecoveryDocument(splitRecoveryWriteBatch+1, splitRecoveryAppended))
	fixture.retry(appended, splitRecoveryAppendedPrint)
	withAppended := fixture.requireKept(stopped, stoppedBits)
	fixture.requireComplete(withAppended, complete)
	if len(withAppended) != len(complete)+1 {
		t.Fatalf("Ingestion stored %d rows; the test expected %d.", len(withAppended), len(complete)+1)
	}
}

func sortedSplitRecoveryIDs(rows map[string]splitRecoveryRow) []string {
	return slices.Sorted(maps.Keys(rows))
}

// splitRecoveryGate forwards embedding requests to the real endpoint after the test
// opens the gate. It stops waiting when the request ends.
type splitRecoveryGate struct {
	mutex   sync.Mutex
	release chan struct{}
	opened  bool
	blocked atomic.Int64
	baseURL string
}

func startSplitRecoveryGate(t *testing.T) *splitRecoveryGate {
	t.Helper()
	target, err := url.Parse(integrationEmbeddingBaseURL())
	if err != nil {
		t.Fatalf("parse the embedding endpoint: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: target.Scheme, Host: target.Host})
	gate := &splitRecoveryGate{mutex: sync.Mutex{}, release: make(chan struct{}), opened: false, blocked: atomic.Int64{}, baseURL: ""}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gate.blocked.Add(1)
		gate.mutex.Lock()
		release := gate.release
		gate.mutex.Unlock()
		select {
		case <-release:
			proxy.ServeHTTP(writer, request)
		case <-request.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(gate.open)
	gate.baseURL = server.URL + target.Path
	return gate
}

func (g *splitRecoveryGate) open() {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if !g.opened {
		close(g.release)
		g.opened = true
	}
}

// pause makes later embedding requests wait and resets the request count.
func (g *splitRecoveryGate) pause() {
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.opened {
		g.release = make(chan struct{})
		g.opened = false
	}
	g.blocked.Store(0)
}

func (g *splitRecoveryGate) waitForRequest(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(splitRecoveryWaitLimit)
	for g.blocked.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upsert did not send an embedding request before the limit")
		}
		time.Sleep(splitRecoveryPollDelay)
	}
}

func readStoredWriteRecords(t *testing.T, fixture *splitRecoveryFixture) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fixture.checkpointDir, writeRecordFolder, fixture.collectionName, "*.json"))
	if err != nil {
		t.Fatalf("list write records: %v", err)
	}
	records := make([]map[string]any, 0, len(paths))
	for _, path := range paths {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read write record: %v", readErr)
		}
		var record map[string]any
		if decodeErr := json.Unmarshal(content, &record); decodeErr != nil {
			t.Fatalf("decode write record: %v", decodeErr)
		}
		records = append(records, record)
	}
	return records
}

func newSplitRecoveryFixture(t *testing.T) *splitRecoveryFixture {
	t.Helper()
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	reader, err := milvusclient.New(context.Background(), &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("connect to Milvus: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close(context.Background()) })
	return &splitRecoveryFixture{
		t: t, address: address, checkpointDir: t.TempDir(), reader: reader, docs: splitRecoveryDocuments(),
		manifest:       []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}},
		collectionName: vectorsearch.CollectionName(splitRecoveryCollectionID),
	}
}

func TestSharedLayoutInFlightUpsertStoresWriteRecord(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	daemon := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	if _, err := daemon.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, fixture.docs, fixture.manifest); err != nil {
		t.Fatalf("write the complete conversation: %v", err)
	}
	complete := fixture.rows()
	if records := readStoredWriteRecords(t, fixture); len(records) != 0 {
		t.Fatalf("The store still contains write records %v after the complete write.", records)
	}

	edited := slices.Clone(fixture.docs)
	longText := edited[splitRecoveryWriteBatch-1].Text
	edited[splitRecoveryWriteBatch-1].Text = longText[:len(longText)-len(splitRecoveryEditedTail)] + splitRecoveryEditedTail
	gate := startSplitRecoveryGate(t)
	backfill := openClientThrough(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir, gate.baseURL)
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := backfill.UpsertConversationDocuments(running, splitRecoveryCollectionID, edited, fixture.manifest)
		finished <- err
	}()
	gate.waitForRequest(t)

	records := readStoredWriteRecords(t, fixture)
	if len(records) != 1 || records[0]["conversation_id"] != splitRecoveryConversationID || records[0]["fingerprint"] != splitRecoveryFingerprint || records[0]["write_id"] == "" {
		t.Fatalf("The running upsert has unexpected write records %v.", records)
	}

	needed, err := daemon.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed contains %v while another client runs the write.", needed)
	}
	_, err = daemon.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, edited, fixture.manifest)
	if !errors.Is(err, vectorsearch.ErrConversationWriteBusy) {
		t.Fatalf("The second client returned error %v; the test expected %v.", err, vectorsearch.ErrConversationWriteBusy)
	}
	other := splitRecoveryDocument(0, splitRecoveryOtherPrint)
	other.ConversationID = splitRecoveryOtherChat
	otherManifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryOtherChat, Value: splitRecoveryOtherPrint}}
	if _, err := daemon.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, []semsearch.SemDoc{other}, otherManifest); err != nil {
		t.Fatalf("write an unrelated conversation: %v", err)
	}
	if records := readStoredWriteRecords(t, fixture); len(records) != 1 {
		t.Fatalf("the second client changed the write records: %v", records)
	}

	cancel()
	if err := <-finished; err == nil {
		t.Fatal("the canceled upsert returned success")
	}
	if records := readStoredWriteRecords(t, fixture); len(records) != 1 {
		t.Fatalf("the canceled upsert removed its write record: %v", records)
	}

	reopened := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	needed, err = reopened.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if !slices.Equal(needed, []string{splitRecoveryConversationID}) {
		t.Fatalf("needed contains %v despite a stopped write with the same fingerprint.", needed)
	}
	if _, err := reopened.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, edited, fixture.manifest); err != nil {
		t.Fatalf("retry the stopped write: %v", err)
	}
	needed, err = reopened.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed still contains %v after the retry.", needed)
	}
	if records := readStoredWriteRecords(t, fixture); len(records) != 0 {
		t.Fatalf("The store still contains write records %v after the retry.", records)
	}
	current := fixture.rows()
	for id, want := range complete {
		if got, found := current[id]; !found || got != want {
			t.Fatalf("The retry changed stored row %s to %+v; the test expected %+v.", id, got, want)
		}
	}
	editedRow := slices.ContainsFunc(slices.Collect(maps.Values(current)), func(row splitRecoveryRow) bool {
		return strings.HasSuffix(row.content, splitRecoveryEditedTail)
	})
	if !editedRow {
		t.Fatal("the retry did not store the edited piece")
	}
}

// TestSharedLayoutRetryKeepsVectorOfWhitespacePiece checks a stopped write with a
// stored whitespace-only piece. Another row stores matching text with a different
// vector. The retry writes the missing piece and preserves the whitespace piece's
// exact vector.
func TestSharedLayoutRetryKeepsVectorOfWhitespacePiece(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	client := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	budget := vectorsearch.ByteBudgetForTest(client)
	text := strings.Repeat("a", budget) + strings.Repeat(" ", budget) + strings.Repeat("b", splitRecoveryTailBytes)
	doc := splitRecoveryDocument(0, text)
	whitespaceID, missingID, err := vectorsearch.WriteWhitespacePieceRowsForTest(ctx, client, splitRecoveryCollectionID, doc, splitRecoveryOtherMessage)
	if err != nil {
		t.Fatalf("write the stored rows: %v", err)
	}
	before := fixture.rows()
	beforeBits := fixture.vectorBits(before)
	whitespaceKeys := make(map[string]struct{})
	for _, row := range before {
		if strings.TrimSpace(row.content) == "" {
			whitespaceKeys[row.vectorKey] = struct{}{}
		}
	}
	if len(whitespaceKeys) != splitRecoveryWhitespaceVectors {
		t.Fatalf("The whitespace rows have %d vectors; the test expected %d.", len(whitespaceKeys), splitRecoveryWhitespaceVectors)
	}
	if err := vectorsearch.WriteStoppedRecordForTest(fixture.checkpointDir, splitRecoveryCollectionID, splitRecoveryConversationID, splitRecoveryFingerprint, integrationModel, []semsearch.SemDoc{doc}); err != nil {
		t.Fatalf("write the stopped write record: %v", err)
	}
	retry := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	if _, err := retry.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, []semsearch.SemDoc{doc}, fixture.manifest); err != nil {
		t.Fatalf("retry the conversation: %v", err)
	}
	after := fixture.requireKept(before, beforeBits)
	if after[whitespaceID].vectorKey != before[whitespaceID].vectorKey {
		t.Fatalf("the retry changed the vector key of the whitespace-only piece")
	}
	if _, found := after[missingID]; !found {
		t.Fatalf("the retry did not write piece %s", missingID)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("The retry stored %d rows instead of %d.", len(after), len(before)+1)
	}
}

func startSplitRecoveryUpsert(ctx context.Context, t *testing.T, client *vectorsearch.Client, gate *splitRecoveryGate, collectionID string, docs []semsearch.SemDoc, manifest []semsearch.Fingerprint) chan error {
	t.Helper()
	finished := make(chan error, 1)
	go func() {
		_, err := client.UpsertConversationDocuments(ctx, collectionID, docs, manifest)
		finished <- err
	}()
	gate.waitForRequest(t)
	return finished
}

// TestSharedLayoutCommittedWriteIDSurvivesStatFailure writes a checkpoint while folder
// permissions prevent record removal. A later upsert writes a checkpoint while
// permissions prevent file lookup and preserves the finished write ID. After permissions
// change, a manifest sync removes both records and their checkpoint IDs.
func TestSharedLayoutCommittedWriteIDSurvivesStatFailure(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	docs := []semsearch.SemDoc{splitRecoveryDocument(0, splitRecoveryFinalMessage)}
	folder := filepath.Join(fixture.checkpointDir, writeRecordFolder, fixture.collectionName)
	t.Cleanup(func() {
		if err := os.Chmod(folder, splitRecoveryFolderMode); err != nil {
			t.Errorf("The test could not make the record folder writable: %v.", err)
		}
	})

	firstGate := startSplitRecoveryGate(t)
	first := openClientThrough(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir, firstGate.baseURL)
	firstFinished := startSplitRecoveryUpsert(ctx, t, first, firstGate, splitRecoveryCollectionID, docs, fixture.manifest)
	records := readStoredWriteRecords(t, fixture)
	if len(records) != 1 {
		t.Fatalf("The running upsert has write records %v; the test expected one record.", records)
	}
	firstWriteID, isString := records[0]["write_id"].(string)
	if !isString {
		t.Fatalf("The write record stores write ID %v instead of a string.", records[0]["write_id"])
	}
	if err := os.Chmod(folder, splitRecoveryReadOnlyFolder); err != nil {
		t.Fatalf("The test could not make the record folder read-only: %v.", err)
	}
	firstGate.open()
	if err := <-firstFinished; err != nil {
		t.Fatalf("The test could not finish the first upsert: %v.", err)
	}
	if committed := fixture.committedWrites(); !slices.Equal(committed, []string{firstWriteID}) {
		t.Fatalf("The checkpoint lists committed writes %v; the test expected %s.", committed, firstWriteID)
	}
	if err := os.Chmod(folder, splitRecoveryFolderMode); err != nil {
		t.Fatalf("The test could not make the record folder writable: %v.", err)
	}

	edited := []semsearch.SemDoc{splitRecoveryDocument(0, splitRecoveryAppended)}
	secondGate := startSplitRecoveryGate(t)
	second := openClientThrough(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir, secondGate.baseURL)
	secondFinished := startSplitRecoveryUpsert(ctx, t, second, secondGate, splitRecoveryCollectionID, edited, fixture.manifest)
	if err := os.Chmod(folder, splitRecoveryNoAccessFolder); err != nil {
		t.Fatalf("The test could not make the record folder unsearchable: %v.", err)
	}
	secondGate.open()
	if err := <-secondFinished; err != nil {
		t.Fatalf("The test could not finish the second upsert: %v.", err)
	}
	if committed := fixture.committedWrites(); !slices.Contains(committed, firstWriteID) {
		t.Fatalf("The checkpoint lists committed writes %v after a stat failure; the test expected %s.", committed, firstWriteID)
	}

	if err := os.Chmod(folder, splitRecoveryFolderMode); err != nil {
		t.Fatalf("The test could not make the record folder searchable: %v.", err)
	}
	reopened := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	needed, err := reopened.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed contains %v after the checkpoint.", needed)
	}
	if remaining := readStoredWriteRecords(t, fixture); len(remaining) != 0 {
		t.Fatalf("The manifest sync did not remove the finished records %v.", remaining)
	}
	if committed := fixture.committedWrites(); len(committed) != 0 {
		t.Fatalf("The checkpoint still lists committed writes %v after the removal.", committed)
	}
}

func (f *splitRecoveryFixture) committedWrites() []string {
	f.t.Helper()
	digest := sha256.Sum256([]byte(splitRecoveryConversationID))
	name := hex.EncodeToString(digest[:])[:splitRecoveryNameChars] + ".json"
	content, err := os.ReadFile(filepath.Join(f.checkpointDir, f.collectionName, name))
	if err != nil {
		f.t.Fatalf("The test could not read the checkpoint: %v.", err)
	}
	var checkpoint struct {
		CommittedWrites []string `json:"committed_writes"`
	}
	if err := json.Unmarshal(content, &checkpoint); err != nil {
		f.t.Fatalf("The test could not decode the checkpoint: %v.", err)
	}
	return checkpoint.CommittedWrites
}

// TestSharedLayoutInMemoryCheckpointKeepsRunningWriteDistinct runs overlapping upserts on
// a client without a checkpoint folder. The store does not classify the running write as
// stopped. After the test cancels the running upsert, the conversation needs ingestion.
func TestSharedLayoutInMemoryCheckpointKeepsRunningWriteDistinct(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	gate := startSplitRecoveryGate(t)
	gate.open()
	client := openClientThrough(t, fixture.address, vectorsearch.LayoutShared, "", gate.baseURL)
	docs := []semsearch.SemDoc{splitRecoveryDocument(0, splitRecoveryFinalMessage)}
	if _, err := client.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, docs, fixture.manifest); err != nil {
		t.Fatalf("The test could not write the conversation: %v.", err)
	}

	gate.pause()
	edited := []semsearch.SemDoc{splitRecoveryDocument(0, splitRecoveryAppended)}
	running, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := startSplitRecoveryUpsert(running, t, client, gate, splitRecoveryCollectionID, edited, fixture.manifest)
	needed, err := client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed contains %v while the write runs.", needed)
	}
	if _, err := client.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, edited, fixture.manifest); !errors.Is(err, vectorsearch.ErrConversationWriteBusy) {
		t.Fatalf("The overlapping upsert returned error %v; the test expected %v.", err, vectorsearch.ErrConversationWriteBusy)
	}

	cancel()
	if err := <-finished; err == nil {
		t.Fatal("The canceled upsert returned success.")
	}
	needed, err = client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if !slices.Equal(needed, []string{splitRecoveryConversationID}) {
		t.Fatalf("needed contains %v; the test expected the stopped conversation.", needed)
	}

	gate.open()
	if _, err := client.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, edited, fixture.manifest); err != nil {
		t.Fatalf("The test could not retry the stopped write: %v.", err)
	}
	needed, err = client.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed still contains %v after the retry.", needed)
	}
}

func recordedModelProof(t *testing.T, address string, model string) string {
	t.Helper()
	const conversationID = "codex:proof"
	checkpoints := t.TempDir()
	gate := startSplitRecoveryGate(t)
	client := openModelClientThrough(t, address, vectorsearch.LayoutShared, checkpoints, gate.baseURL, model)
	doc := semsearch.SemDoc{
		ConversationID: conversationID, ParentConversationID: "codex:parent", MessageIndex: 3, Role: "user",
		TimestampUnix: 1_790_000_000, Text: "message text", Tools: nil, Thinking: "",
		WorkspaceRoot: "/work", Archived: false, LoadRules: "rules",
	}
	manifest := []semsearch.Fingerprint{{ConversationID: conversationID, Value: splitRecoveryFingerprint}}
	running, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := startSplitRecoveryUpsert(running, t, client, gate, splitRecoveryCollectionID, []semsearch.SemDoc{doc}, manifest)
	fixture := &splitRecoveryFixture{t: t, checkpointDir: checkpoints, collectionName: vectorsearch.CollectionName(splitRecoveryCollectionID)}
	records := readStoredWriteRecords(t, fixture)
	cancel()
	<-finished
	if len(records) != 1 {
		t.Fatalf("The running upsert has write records %v; the test expected one record.", records)
	}
	families, isList := records[0]["families"].([]any)
	if !isList || len(families) != 1 {
		t.Fatalf("The write record stores families %v; the test expected one family.", records[0]["families"])
	}
	family, isMap := families[0].(map[string]any)
	if !isMap {
		t.Fatalf("The write record stores family %v instead of a JSON object.", families[0])
	}
	proof, isString := family["proof"].(string)
	if !isString {
		t.Fatalf("The write record stores proof %v instead of a string.", family["proof"])
	}
	return proof
}

func TestSharedLayoutWriteRecordProofCoversEmbeddingModel(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	proof := recordedModelProof(t, address, splitRecoveryProofModel)
	if proof != splitRecoveryStoredProof {
		t.Fatalf("The write record has proof %s; the test expected %s.", proof, splitRecoveryStoredProof)
	}
	if other := recordedModelProof(t, address, splitRecoveryOtherProofModel); other == proof {
		t.Fatal("The proof did not change with the embedding model.")
	}
}

// TestSharedLayoutTwoClientsRecoverOneStoppedWrite pauses the first client's
// embedding request during recovery. The second client receives a busy error before
// reading the stopped write. The first client then finishes recovery.
func TestSharedLayoutTwoClientsRecoverOneStoppedWrite(t *testing.T) {
	fixture := newSplitRecoveryFixture(t)
	ctx := context.Background()
	writer := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	if _, err := writer.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, fixture.docs, fixture.manifest); err != nil {
		t.Fatalf("write the complete conversation: %v", err)
	}
	complete := fixture.rows()
	fixture.stopWrite(complete)
	fixture.markStopped()

	gate := startSplitRecoveryGate(t)
	first := openClientThrough(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir, gate.baseURL)
	finished := make(chan error, 1)
	go func() {
		_, err := first.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, fixture.docs, fixture.manifest)
		finished <- err
	}()
	gate.waitForRequest(t)

	second := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	_, err := second.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, fixture.docs, fixture.manifest)
	if !errors.Is(err, vectorsearch.ErrConversationWriteBusy) {
		t.Fatalf("The second client returned error %v; the test expected %v.", err, vectorsearch.ErrConversationWriteBusy)
	}

	gate.open()
	if err := <-finished; err != nil {
		t.Fatalf("finish the recovery: %v", err)
	}
	recovered := fixture.rows()
	if !slices.Equal(sortedSplitRecoveryIDs(recovered), sortedSplitRecoveryIDs(complete)) {
		t.Fatalf("The recovery stored IDs %v; the test expected %v.", sortedSplitRecoveryIDs(recovered), sortedSplitRecoveryIDs(complete))
	}
	if records := readStoredWriteRecords(t, fixture); len(records) != 0 {
		t.Fatalf("The store still contains write records %v after recovery.", records)
	}
	needed, err := second.SyncConversationManifest(ctx, splitRecoveryCollectionID, fixture.manifest)
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed still contains %v after recovery.", needed)
	}
}
