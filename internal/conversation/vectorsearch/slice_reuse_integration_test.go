//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const (
	sliceReuseMessages   = 600
	sliceReuseFirstSlice = 256
	sliceReuseMovedPrint = "moved"
	// sliceBoundaryRow is the index of the first slice's last row at the default row limit.
	sliceBoundaryRow      = 255
	sliceBoundaryMessages = 300
	sliceBoundaryText     = "slice boundary repeated text about a harbor survey"
	// sliceReuseEditedMessage selects the message that starts on the first slice's last row.
	sliceReuseEditedMessage = sliceReuseFirstSlice - 1
	// sliceReuseReadMargin allows one embedding batch of extra reads in each of two slices.
	sliceReuseReadMargin      = 64
	sliceReuseTimestampColumn = "timestampUnix"
	sliceReuseMissingVector   = "references missing vector"
	// sliceReuseDenseWord uses about one token per two bytes. Repeating it produces text
	// that fits the byte budget but exceeds the real embedding model's token limit.
	sliceReuseDenseWord  = "a "
	sliceReuseDenseWords = 4500
	sliceReuseTimeChange = 3600
)

func sliceReuseDocuments(shift int) []semsearch.SemDoc {
	docs := make([]semsearch.SemDoc, 0, sliceReuseMessages)
	for position := range sliceReuseMessages {
		doc := splitRecoveryDocument(position+shift, fmt.Sprintf("slice reuse message %04d about a harbor survey", position))
		doc.TimestampUnix = splitRecoveryTimestamp + int64(position)
		docs = append(docs, doc)
	}
	return docs
}

// sliceReuseReversedDocuments reverses message text order. Later slices read source
// rows at short paths such as /1, /10, and /100.
func sliceReuseReversedDocuments() []semsearch.SemDoc {
	docs := sliceReuseDocuments(0)
	reversed := slices.Clone(docs)
	for position := range docs {
		reversed[position].Text = docs[len(docs)-1-position].Text
	}
	return reversed
}

func newSliceReuseFixture(t *testing.T) *splitRecoveryFixture {
	t.Helper()
	milvustest.RequireEnabled(t)
	ctx := context.Background()
	address := milvustest.Start(t)
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("connect to Milvus: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close(ctx) })
	return &splitRecoveryFixture{
		t: t, address: address, checkpointDir: t.TempDir(), reader: reader, docs: nil, manifest: nil,
		collectionName: vectorsearch.CollectionName(splitRecoveryCollectionID),
	}
}

// runSliceToCheckpoint runs slices with one client until the upsert returns success.
func runSliceToCheckpoint(t *testing.T, client *vectorsearch.Client, docs []semsearch.SemDoc, fingerprint string) {
	t.Helper()
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: fingerprint}}
	for attempt := 0; attempt <= sliceReuseMessages; attempt++ {
		_, err := client.UpsertConversationSlice(context.Background(), splitRecoveryCollectionID, docs, manifest)
		if err == nil {
			return
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(err, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
		}
	}
	t.Fatal("the conversation did not finish in the expected slice count")
}

func rowTimestamps(t *testing.T, fixture *splitRecoveryFixture) map[string]int64 {
	t.Helper()
	result, err := fixture.reader.Query(context.Background(), milvusclient.NewQueryOption(fixture.collectionName).
		WithFilter(fmt.Sprintf("%s == %q", splitRecoveryConversation, splitRecoveryConversationID)).
		WithOutputFields(milvusstore.IDField, sliceReuseTimestampColumn).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		t.Fatalf("read stored timestamps: %v", err)
	}
	timestamps := make(map[string]int64, result.ResultCount)
	for position := range result.ResultCount {
		id, idErr := result.GetColumn(milvusstore.IDField).GetAsString(position)
		timestamp, timestampErr := result.GetColumn(sliceReuseTimestampColumn).GetAsInt64(position)
		if idErr != nil || timestampErr != nil {
			t.Fatalf("decode stored timestamp %d: %v %v", position, idErr, timestampErr)
		}
		timestamps[id] = timestamp
	}
	return timestamps
}

// TestSharedLayoutSliceReportsMissingStoredVector deletes a planned source row's
// vector in real Milvus. The next slice returns an error and does not embed any vectors.
func TestSharedLayoutSliceReportsMissingStoredVector(t *testing.T) {
	fixture := newSliceReuseFixture(t)
	ctx := context.Background()
	storedManifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}}
	first := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	if _, err := first.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, sliceReuseDocuments(0), storedManifest); err != nil {
		t.Fatalf("store the first version: %v", err)
	}
	keyByContent := make(map[string]string, sliceReuseMessages)
	for _, row := range fixture.rows() {
		keyByContent[row.content] = row.vectorKey
	}

	client := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	moved := sliceReuseDocuments(1)
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: sliceReuseMovedPrint}}
	_, err := client.UpsertConversationSlice(ctx, splitRecoveryCollectionID, moved, manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The first slice returned error %v; the test expected a partial upsert.", err)
	}
	// The first slice has finished. The next slice needs this row's source vector.
	nextText := moved[sliceReuseFirstSlice].Text
	vectors := milvusstore.VectorCollectionName(fixture.collectionName)
	if _, err := fixture.reader.Delete(ctx, milvusclient.NewDeleteOption(vectors).
		WithExpr(fmt.Sprintf("%s == %q", milvusstore.VectorKeyField, keyByContent[nextText]))); err != nil {
		t.Fatalf("delete the stored vector: %v", err)
	}
	_, err = client.UpsertConversationSlice(ctx, splitRecoveryCollectionID, moved, manifest)
	if err == nil || !strings.Contains(err.Error(), sliceReuseMissingVector) {
		t.Fatalf("The slice returned error %v; the test expected %q.", err, sliceReuseMissingVector)
	}
	if embedded := client.IngestStatus(splitRecoveryCollectionID).LastUpsert.VectorsEmbedded; embedded != 0 {
		t.Fatalf("The slice embedded %d vectors; the test expected 0.", embedded)
	}
}

// TestSharedLayoutSliceReusesRowsWithNullModel clears stored rows' embedding models
// in real Milvus. Slices accept null models as compatible and do not embed any vectors.
func TestSharedLayoutSliceReusesRowsWithNullModel(t *testing.T) {
	fixture := newSliceReuseFixture(t)
	ctx := context.Background()
	storedManifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}}
	first := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	if _, err := first.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, sliceReuseDocuments(0), storedManifest); err != nil {
		t.Fatalf("store the first version: %v", err)
	}
	ids := slices.Sorted(maps.Keys(fixture.rows()))
	models, err := column.NewNullableColumnVarChar(milvusstore.EmbeddingModelField, []string{}, make([]bool, len(ids)))
	if err != nil {
		t.Fatalf("build the null model column: %v", err)
	}
	option := milvusclient.NewColumnBasedInsertOption(fixture.collectionName).
		WithVarcharColumn(milvusstore.IDField, ids).
		WithColumns(models).
		WithPartialUpdate(true)
	if _, err := fixture.reader.Upsert(ctx, option); err != nil {
		t.Fatalf("clear the stored embedding models: %v", err)
	}

	client := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	moved := sliceReuseDocuments(1)
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: sliceReuseMovedPrint}}
	embedded := 0
	for attempt := 0; ; attempt++ {
		if attempt > sliceReuseMessages {
			t.Fatal("the conversation did not finish in the expected slice count")
		}
		_, upsertErr := client.UpsertConversationSlice(ctx, splitRecoveryCollectionID, moved, manifest)
		embedded += client.IngestStatus(splitRecoveryCollectionID).LastUpsert.VectorsEmbedded
		if upsertErr == nil {
			break
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(upsertErr, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", upsertErr)
		}
	}
	if embedded != 0 {
		t.Fatalf("The slices embedded %d vectors; the test expected 0.", embedded)
	}
}

// TestSharedLayoutSliceKeepsVectorBytesAcrossRestart stops after one slice and
// finishes with a new client. The test verifies that the retry preserves the first
// slice's exact vector bytes.
func TestSharedLayoutSliceKeepsVectorBytesAcrossRestart(t *testing.T) {
	fixture := newSliceReuseFixture(t)
	docs := sliceReuseDocuments(0)
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}}
	first := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	_, err := first.UpsertConversationSlice(context.Background(), splitRecoveryCollectionID, docs, manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The first slice returned error %v; the test expected a partial upsert.", err)
	}
	kept := fixture.rows()
	if len(kept) == 0 || len(kept) == sliceReuseMessages {
		t.Fatalf("The first slice stored %d rows; the test expected a partial write.", len(kept))
	}
	keptBits := fixture.vectorBits(kept)

	restarted := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	runSliceToCheckpoint(t, restarted, docs, splitRecoveryFingerprint)
	if current := fixture.requireKept(kept, keptBits); len(current) != sliceReuseMessages {
		t.Fatalf("The finished conversation has %d stored rows; the test expected %d.", len(current), sliceReuseMessages)
	}
}

// TestSharedLayoutSliceKeepsVectorBytesForEditedMessage edits the end of a message
// after the first slice stores part of it. A new client writes the new end while
// preserving the saved piece's exact vector bytes.
func TestSharedLayoutSliceKeepsVectorBytesForEditedMessage(t *testing.T) {
	fixture := newSliceReuseFixture(t)
	docs := slices.Clone(sliceReuseDocuments(0))
	docs[sliceReuseEditedMessage].Text = splitRecoveryLongText()
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}}
	first := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	_, err := first.UpsertConversationSlice(context.Background(), splitRecoveryCollectionID, docs, manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The first slice returned error %v; the test expected a partial upsert.", err)
	}
	kept := fixture.rows()
	savedPieces := splitMessagePieces(kept, docs[sliceReuseEditedMessage].Text)
	if savedPieces == 0 {
		t.Fatal("the first slice did not save a piece of the split message")
	}
	keptBits := fixture.vectorBits(kept)

	edited := slices.Clone(docs)
	edited[sliceReuseEditedMessage].Text = docs[sliceReuseEditedMessage].Text + splitRecoveryEdit
	restarted := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	runSliceToCheckpoint(t, restarted, edited, splitRecoveryEditedPrint)
	current := fixture.requireKept(kept, keptBits)
	if pieces := splitMessagePieces(current, edited[sliceReuseEditedMessage].Text); pieces <= savedPieces {
		t.Fatalf("The split message has %d stored pieces after the edit and %d before it; the test expected the count to increase.", pieces, savedPieces)
	}
	foundEdit := false
	for _, row := range current {
		foundEdit = foundEdit || strings.HasSuffix(row.content, splitRecoveryEdit)
	}
	if !foundEdit {
		t.Fatal("the stored rows do not include the new end of the edited message")
	}
}

func splitMessagePieces(rows map[string]splitRecoveryRow, text string) int {
	pieces := 0
	for _, row := range rows {
		if strings.Contains(text, row.content) {
			pieces++
		}
	}
	return pieces
}

// TestSharedLayoutEndpointSplitKeepsSavedChild sends a message that exceeds the real
// endpoint's token limit. It removes one split child and the checkpoint, then retries
// with a new client and changed source timestamp. The retry restores the removed
// child and preserves the saved child's timestamp and exact vector bytes.
func TestSharedLayoutEndpointSplitKeepsSavedChild(t *testing.T) {
	fixture := newSliceReuseFixture(t)
	doc := splitRecoveryDocument(0, strings.Repeat(sliceReuseDenseWord, sliceReuseDenseWords))
	docs := []semsearch.SemDoc{doc}
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}}
	client := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	if _, err := client.UpsertConversationDocuments(context.Background(), splitRecoveryCollectionID, docs, manifest); err != nil {
		t.Fatalf("store the split message: %v", err)
	}
	complete := fixture.rows()
	if len(complete) < splitRecoveryMinimumPieces {
		t.Fatalf("The endpoint split produced %d rows; the test expected at least %d.", len(complete), splitRecoveryMinimumPieces)
	}
	ids := slices.Sorted(maps.Keys(complete))
	removedID := ids[len(ids)-1]
	if _, err := fixture.reader.Delete(context.Background(), milvusclient.NewDeleteOption(fixture.collectionName).
		WithExpr(fmt.Sprintf("%s in [%q]", milvusstore.IDField, removedID))); err != nil {
		t.Fatalf("remove one split child: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(fixture.checkpointDir, fixture.collectionName)); err != nil {
		t.Fatalf("remove the test checkpoint: %v", err)
	}
	if err := vectorsearch.WriteStoppedRecordForTest(fixture.checkpointDir, splitRecoveryCollectionID, splitRecoveryConversationID, splitRecoveryFingerprint, integrationModel, docs); err != nil {
		t.Fatalf("write the stopped write record: %v", err)
	}
	kept := fixture.rows()
	keptBits := fixture.vectorBits(kept)
	keptTimes := rowTimestamps(t, fixture)

	changed := doc
	changed.TimestampUnix = doc.TimestampUnix + sliceReuseTimeChange
	restarted := openClientWithCheckpoints(t, fixture.address, vectorsearch.LayoutShared, fixture.checkpointDir)
	retried := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryEditedPrint}}
	if _, err := restarted.UpsertConversationDocuments(context.Background(), splitRecoveryCollectionID, []semsearch.SemDoc{changed}, retried); err != nil {
		t.Fatalf("retry the split message: %v", err)
	}
	current := fixture.requireKept(kept, keptBits)
	if _, restored := current[removedID]; !restored {
		t.Fatalf("the retry did not restore split child %s", removedID)
	}
	times := rowTimestamps(t, fixture)
	for id, want := range keptTimes {
		if times[id] != want {
			t.Fatalf("the retry changed the timestamp of row %s from %d to %d", id, want, times[id])
		}
	}
}

// TestSharedLayoutSliceReusesVectorAcrossBoundary writes matching text on the first
// slice's last row and the second slice's first row in real Milvus. The second slice
// reads the first row's stored vector. Both rows store the same vector key.
func TestSharedLayoutSliceReusesVectorAcrossBoundary(t *testing.T) {
	milvustest.RequireEnabled(t)
	ctx := context.Background()
	address := milvustest.Start(t)
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("connect to Milvus: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close(ctx) })
	checkpoints := t.TempDir()
	fixture := &splitRecoveryFixture{
		t: t, address: address, checkpointDir: checkpoints, reader: reader, docs: nil, manifest: nil,
		collectionName: vectorsearch.CollectionName(splitRecoveryCollectionID),
	}
	docs := make([]semsearch.SemDoc, 0, sliceBoundaryMessages)
	for position := range sliceBoundaryMessages {
		text := fmt.Sprintf("slice boundary message %04d about a harbor survey", position)
		if position == sliceBoundaryRow+1 || position == sliceBoundaryRow {
			text = sliceBoundaryText
		}
		docs = append(docs, splitRecoveryDocument(position, text))
	}
	client := openClientWithCheckpoints(t, address, vectorsearch.LayoutShared, checkpoints)
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}}
	embedded := 0
	partials := 0
	for attempt := 0; ; attempt++ {
		if attempt > sliceBoundaryMessages {
			t.Fatal("the conversation did not finish in the expected slice count")
		}
		_, upsertErr := client.UpsertConversationSlice(ctx, splitRecoveryCollectionID, docs, manifest)
		embedded += client.IngestStatus(splitRecoveryCollectionID).LastUpsert.VectorsEmbedded
		if upsertErr == nil {
			break
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(upsertErr, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", upsertErr)
		}
		partials++
	}
	if partials == 0 {
		t.Fatal("the conversation finished in one slice")
	}
	if embedded != sliceBoundaryMessages-1 {
		t.Fatalf("The slices embedded %d vectors; the test expected %d.", embedded, sliceBoundaryMessages-1)
	}
	rows := fixture.rows()
	if len(rows) != sliceBoundaryMessages {
		t.Fatalf("The slices stored %d rows; the test expected %d.", len(rows), sliceBoundaryMessages)
	}
	keys := make([]string, 0, 2)
	for _, row := range rows {
		if row.content == sliceBoundaryText {
			keys = append(keys, row.vectorKey)
		}
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("The rows with matching text store vector keys %v; the test expected two equal keys.", keys)
	}
}

// TestSharedLayoutSlicesReuseStoredVectors reverses message text order in real Milvus.
// Each planned row has a matching stored vector. Later slices read source rows at
// paths that prefix other paths. The slices do not embed any vectors. They respect
// the read limit and preserve existing vector keys and bytes.
func TestSharedLayoutSlicesReuseStoredVectors(t *testing.T) {
	milvustest.RequireEnabled(t)
	ctx := context.Background()
	address := milvustest.Start(t)
	reader, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatalf("connect to Milvus: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close(ctx) })
	checkpoints := t.TempDir()
	fixture := &splitRecoveryFixture{
		t: t, address: address, checkpointDir: checkpoints, reader: reader, docs: nil, manifest: nil,
		collectionName: vectorsearch.CollectionName(splitRecoveryCollectionID),
	}

	first := openClientWithCheckpoints(t, address, vectorsearch.LayoutShared, checkpoints)
	storedManifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: splitRecoveryFingerprint}}
	if _, err := first.UpsertConversationDocuments(ctx, splitRecoveryCollectionID, sliceReuseDocuments(0), storedManifest); err != nil {
		t.Fatalf("store the first version: %v", err)
	}
	before := fixture.rows()
	if len(before) != sliceReuseMessages {
		t.Fatalf("The first version stored %d rows; the test expected %d.", len(before), sliceReuseMessages)
	}
	keyByContent := make(map[string]string, len(before))
	for _, row := range before {
		keyByContent[row.content] = row.vectorKey
	}
	bitsBefore := fixture.vectorBits(before)

	client := openClientWithCheckpoints(t, address, vectorsearch.LayoutShared, checkpoints)
	moved := sliceReuseReversedDocuments()
	manifest := []semsearch.Fingerprint{{ConversationID: splitRecoveryConversationID, Value: sliceReuseMovedPrint}}
	rowsRead := 0
	embedded := 0
	for attempt := 0; ; attempt++ {
		if attempt > sliceReuseMessages {
			t.Fatal("the conversation did not finish in the expected slice count")
		}
		_, upsertErr := client.UpsertConversationSlice(ctx, splitRecoveryCollectionID, moved, manifest)
		status := client.IngestStatus(splitRecoveryCollectionID).LastUpsert
		rowsRead += status.RowsRead
		embedded += status.VectorsEmbedded
		if upsertErr == nil {
			break
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(upsertErr, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", upsertErr)
		}
	}
	t.Logf("the slices read %d stored rows and embedded %d vectors", rowsRead, embedded)
	if embedded != 0 {
		t.Fatalf("The slices embedded %d vectors; the test expected 0.", embedded)
	}
	// The first and final slices read every stored row. Other slices read one source row
	// per remaining planned row. A prefix query for /1 also selects /10 through /19 and
	// /100 through /199. Those extra rows exceed the read margin.
	maxRows := sliceReuseMessages + (sliceReuseMessages - sliceReuseFirstSlice) + 2*sliceReuseMessages + sliceReuseReadMargin
	if rowsRead > maxRows {
		t.Fatalf("The slices read %d stored rows; the test expected at most %d.", rowsRead, maxRows)
	}

	after := fixture.rows()
	if len(after) != 2*sliceReuseMessages {
		t.Fatalf("The store contains %d rows after the text reorder; the test expected %d.", len(after), 2*sliceReuseMessages)
	}
	for id, row := range after {
		if row.vectorKey != keyByContent[row.content] {
			t.Fatalf("Row %s stores vector key %s; the test expected %s.", id, row.vectorKey, keyByContent[row.content])
		}
	}
	if !maps.EqualFunc(fixture.vectorBits(after), bitsBefore, slices.Equal[[]uint32]) {
		t.Fatal("the slices changed the bytes of a stored vector")
	}
	needed, err := client.SyncConversationManifest(ctx, splitRecoveryCollectionID, manifest)
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if len(needed) != 0 {
		t.Fatalf("The manifest still lists conversations %v for ingestion.", needed)
	}
}
