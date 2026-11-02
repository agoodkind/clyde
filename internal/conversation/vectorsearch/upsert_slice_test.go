package vectorsearch_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	sliceCollectionID   = "upsert-slice"
	sliceConversationID = "codex:upsert-slice"
	sliceRows           = 32
	sliceMessages       = 100
	sliceFingerprint    = "slice-v1"
	sliceChangedPrint   = "slice-v2"

	sliceBlockedConversationID     = "codex:a-upsert-slice-blocked"
	sliceLaterSortedConversationID = "codex:z-upsert-slice-small"
	sliceSplitRepeats              = 6000
	// sliceRequestRows is the largest row count of one embedding request.
	sliceRequestRows = 32
	// slicePlanFitsOne is a byte limit that permits one plan for sliceMessages short messages.
	slicePlanFitsOne         = 8_000
	sliceCheckpointNameChars = 32
	sliceEditedStart         = "edited message"
	sliceEditedFiller        = " harbor survey words for a split message"
	sliceEditedRepeats       = 2000
	sliceEditedEnd           = " edited end of the message"
)

func sliceDocument(position int) semsearch.SemDoc {
	return semsearch.SemDoc{
		ConversationID: sliceConversationID, ParentConversationID: "", MessageIndex: int32(position), Role: "user",
		TimestampUnix: 1_790_000_000 + int64(position), Text: fmt.Sprintf("slice message %04d about a harbor survey", position),
		Tools: nil, Thinking: "", WorkspaceRoot: "", Archived: false, LoadRules: "",
	}
}

func sliceDocuments(count int) []semsearch.SemDoc {
	docs := make([]semsearch.SemDoc, 0, count)
	for position := range count {
		docs = append(docs, sliceDocument(position))
	}
	return docs
}

func openSliceClient(t *testing.T, root string, checkpoints string) *vectorsearch.Client {
	t.Helper()
	client, err := vectorsearch.Open(context.Background(), vectorsearch.Options{
		Backend: vectorsearch.BackendLocal, LocalRoot: root, CheckpointDir: checkpoints, IngestSliceRows: sliceRows,
	})
	if err != nil {
		t.Fatalf("open the local client: %v", err)
	}
	return client
}

func sliceStoredVectors(t *testing.T, root string) map[string]string {
	t.Helper()
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("open the local store: %v", err)
	}
	defer store.Close()
	rows, err := store.QueryRows(context.Background(), collection.RowsRequest{
		Collection: vectorsearch.CollectionName(sliceCollectionID), Declaration: collection.Declaration{ItemIDColumn: "", Scalars: nil},
		ItemIDs: nil, PathPrefixes: []string{"conv/"}, IncludeVector: true,
	})
	if err != nil {
		t.Fatalf("read stored rows: %v", err)
	}
	vectors := make(map[string]string, len(rows))
	for _, row := range rows {
		vectors[row.ID] = fmt.Sprint(row.Vector)
	}
	return vectors
}

func sliceStoredContents(t *testing.T, root string) []string {
	t.Helper()
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("open the local store: %v", err)
	}
	defer store.Close()
	rows, err := store.QueryRows(context.Background(), collection.RowsRequest{
		Collection: vectorsearch.CollectionName(sliceCollectionID), Declaration: collection.Declaration{ItemIDColumn: "", Scalars: nil},
		ItemIDs: nil, PathPrefixes: []string{"conv/"}, IncludeVector: false,
	})
	if err != nil {
		t.Fatalf("read stored rows: %v", err)
	}
	contents := make([]string, 0, len(rows))
	for _, row := range rows {
		contents = append(contents, row.Content)
	}
	return contents
}

// runSliceAfterRestart opens a new client, runs one slice, and closes the client. Each
// call reads the write record and stored rows from disk, as a restarted daemon does.
func runSliceAfterRestart(t *testing.T, root string, checkpoints string, docs []semsearch.SemDoc, fingerprint string) (error, int) {
	t.Helper()
	ctx := context.Background()
	client := openSliceClient(t, root, checkpoints)
	manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: fingerprint}}
	_, err := client.UpsertConversationSlice(ctx, sliceCollectionID, docs, manifest)
	embedded := client.IngestStatus(sliceCollectionID).LastUpsert.VectorsEmbedded
	if closeErr := client.Close(ctx); closeErr != nil {
		t.Fatalf("close the local client: %v", closeErr)
	}
	return err, embedded
}

func requireSliceNeeded(t *testing.T, root string, checkpoints string, fingerprint string, want bool) {
	t.Helper()
	ctx := context.Background()
	client := openSliceClient(t, root, checkpoints)
	defer func() { _ = client.Close(ctx) }()
	needed, err := client.SyncConversationManifest(ctx, sliceCollectionID, []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: fingerprint}})
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if slices.Contains(needed, sliceConversationID) != want {
		t.Fatalf("needed contains %v; the test expected conversation membership to be %t.", needed, want)
	}
}

// TestUpsertConversationSliceFinishesAcrossRestarts uses a new client for each slice.
// The conversation needs ingestion until the final slice. The test adds a source
// message between slices and checks the final checkpoint's current fingerprint.
// Each slice preserves previously stored rows and vectors.
func TestUpsertConversationSliceFinishesAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	checkpoints := t.TempDir()
	docs := sliceDocuments(sliceMessages)
	embeddedTotal := 0

	err, embedded := runSliceAfterRestart(t, root, checkpoints, docs, sliceFingerprint)
	embeddedTotal += embedded
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) || !slices.Equal(partial.Incomplete, []string{sliceConversationID}) {
		t.Fatalf("The first slice returned error %v; the test expected a partial upsert of %s.", err, sliceConversationID)
	}
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, true)
	firstRows := sliceStoredVectors(t, root)
	if len(firstRows) != sliceRows {
		t.Fatalf("The first slice stored %d rows; the test expected %d.", len(firstRows), sliceRows)
	}

	err, embedded = runSliceAfterRestart(t, root, checkpoints, docs, sliceFingerprint)
	embeddedTotal += embedded
	if !errors.As(err, &partial) {
		t.Fatalf("The second slice returned error %v; the test expected a partial upsert.", err)
	}
	secondRows := sliceStoredVectors(t, root)
	for id, vector := range firstRows {
		if secondRows[id] != vector {
			t.Fatalf("the second slice changed stored row %s", id)
		}
	}

	// The source gains a message before the next slice.
	changed := sliceDocuments(sliceMessages + 1)
	for attempt := 0; ; attempt++ {
		if attempt > sliceMessages {
			t.Fatal("the conversation did not finish in the expected slice count")
		}
		err, embedded = runSliceAfterRestart(t, root, checkpoints, changed, sliceChangedPrint)
		embeddedTotal += embedded
		if err == nil {
			break
		}
		if !errors.As(err, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
		}
		requireSliceNeeded(t, root, checkpoints, sliceChangedPrint, true)
	}

	finalRows := sliceStoredVectors(t, root)
	if len(finalRows) != sliceMessages+1 {
		t.Fatalf("The final state has %d rows; the test expected %d.", len(finalRows), sliceMessages+1)
	}
	for id, vector := range secondRows {
		if finalRows[id] != vector {
			t.Fatalf("a later slice changed stored row %s", id)
		}
	}
	if embeddedTotal != sliceMessages+1 {
		t.Fatalf("The slices embedded %d vectors; the test expected %d.", embeddedTotal, sliceMessages+1)
	}
	requireSliceNeeded(t, root, checkpoints, sliceChangedPrint, false)
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, true)
}

// TestUpsertConversationSliceWritesTailOfEditedMessage changes a split message's end
// after the first slice stores its first pieces. Those pieces match the new text's
// start. The conversation needs ingestion until the client stores the new end.
func TestUpsertConversationSliceWritesTailOfEditedMessage(t *testing.T) {
	root := t.TempDir()
	checkpoints := t.TempDir()
	original := sliceDocument(0)
	original.Text = sliceEditedStart + strings.Repeat(sliceEditedFiller, sliceEditedRepeats)
	edited := original
	edited.Text = original.Text + sliceEditedEnd

	err, _ := runSliceAfterRestart(t, root, checkpoints, []semsearch.SemDoc{original}, sliceFingerprint)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The first slice returned error %v; the test expected a partial upsert.", err)
	}
	firstRows := sliceStoredVectors(t, root)
	if len(firstRows) == 0 {
		t.Fatal("the first slice did not store any rows")
	}

	for attempt := 0; ; attempt++ {
		if attempt > sliceMessages {
			t.Fatal("the conversation did not finish in the expected slice count")
		}
		err, _ = runSliceAfterRestart(t, root, checkpoints, []semsearch.SemDoc{edited}, sliceChangedPrint)
		if err == nil {
			break
		}
		if !errors.As(err, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
		}
		requireSliceNeeded(t, root, checkpoints, sliceChangedPrint, true)
	}
	finalRows := sliceStoredVectors(t, root)
	for id, vector := range firstRows {
		if finalRows[id] != vector {
			t.Fatalf("a later slice changed stored row %s", id)
		}
	}
	contents := sliceStoredContents(t, root)
	if !slices.ContainsFunc(contents, func(content string) bool { return strings.HasSuffix(content, sliceEditedEnd) }) {
		t.Fatal("the stored rows do not include the new end of the edited message")
	}
	requireSliceNeeded(t, root, checkpoints, sliceChangedPrint, false)
}

// TestUpsertConversationSliceReportsCheckpointFailureWithPartialChat submits a batch
// with a checkpoint failure and a partial conversation. The partial error reports both.
func TestUpsertConversationSliceReportsCheckpointFailureWithPartialChat(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	digest := sha256.Sum256([]byte(sliceBlockedConversationID))
	blockerName := hex.EncodeToString(digest[:])[:sliceCheckpointNameChars] + ".json"
	blockerPath := filepath.Join(checkpoints, vectorsearch.CollectionName(sliceCollectionID), blockerName)
	if err := os.MkdirAll(filepath.Join(blockerPath, "blocker"), 0o700); err != nil {
		t.Fatalf("create the checkpoint blocker: %v", err)
	}
	blocked := sliceDocument(0)
	blocked.ConversationID = sliceBlockedConversationID
	docs := append([]semsearch.SemDoc{blocked}, sliceDocuments(sliceMessages)...)
	manifest := []semsearch.Fingerprint{
		{ConversationID: sliceBlockedConversationID, Value: sliceFingerprint},
		{ConversationID: sliceConversationID, Value: sliceFingerprint},
	}
	client := openSliceClient(t, root, checkpoints)
	defer func() { _ = client.Close(ctx) }()
	_, err := client.UpsertConversationSlice(ctx, sliceCollectionID, docs, manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
	}
	if !slices.Equal(partial.Failed, []string{sliceBlockedConversationID}) || len(partial.Failures) != 1 {
		t.Fatalf("The upsert reports failed conversations %v with errors %v; the test expected %s.", partial.Failed, partial.Failures, sliceBlockedConversationID)
	}
	if !slices.Equal(partial.Incomplete, []string{sliceConversationID}) {
		t.Fatalf("The upsert reports incomplete conversations %v; the test expected %s.", partial.Incomplete, sliceConversationID)
	}
	if !strings.Contains(err.Error(), sliceBlockedConversationID) {
		t.Fatalf("slice error %q does not include %s", err, sliceBlockedConversationID)
	}
}

// runSlicesToCheckpoint runs slices with one client until the upsert returns success.
// It returns the total counts of stored rows read and vectors embedded.
func runSlicesToCheckpoint(t *testing.T, client *vectorsearch.Client, docs []semsearch.SemDoc, fingerprint string) (int, int) {
	t.Helper()
	return runSlicesToCheckpointFor(t, client, docs, []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: fingerprint}})
}

func runSlicesToCheckpointFor(t *testing.T, client *vectorsearch.Client, docs []semsearch.SemDoc, manifest []semsearch.Fingerprint) (int, int) {
	t.Helper()
	rowsRead := 0
	embedded := 0
	for attempt := 0; ; attempt++ {
		if attempt > sliceMessages {
			t.Fatal("the conversation did not finish in the expected slice count")
		}
		_, err := client.UpsertConversationSlice(context.Background(), sliceCollectionID, docs, manifest)
		status := client.IngestStatus(sliceCollectionID).LastUpsert
		rowsRead += status.RowsRead
		embedded += status.VectorsEmbedded
		if err == nil {
			return rowsRead, embedded
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(err, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
		}
	}
}

// TestUpsertConversationSliceReadsStoredRowsOnce writes a conversation in slices with
// one client. The client reads stored rows once, before recording the checkpoint.
// The test compares stored vectors with the vectors from one whole upsert.
func TestUpsertConversationSliceReadsStoredRowsOnce(t *testing.T) {
	ctx := context.Background()
	slicedRoot := t.TempDir()
	docs := sliceDocuments(sliceMessages)
	sliced := openSliceClient(t, slicedRoot, t.TempDir())
	rowsRead, embedded := runSlicesToCheckpoint(t, sliced, docs, sliceFingerprint)
	if err := sliced.Close(ctx); err != nil {
		t.Fatalf("close the local client: %v", err)
	}
	if rowsRead != sliceMessages {
		t.Fatalf("The slices read %d stored rows; the test expected %d.", rowsRead, sliceMessages)
	}
	if embedded != sliceMessages {
		t.Fatalf("The slices embedded %d vectors; the test expected %d.", embedded, sliceMessages)
	}

	wholeRoot := t.TempDir()
	whole := openSliceClient(t, wholeRoot, t.TempDir())
	manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: sliceFingerprint}}
	if _, err := whole.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest); err != nil {
		t.Fatalf("upsert the whole conversation: %v", err)
	}
	if err := whole.Close(ctx); err != nil {
		t.Fatalf("close the local client: %v", err)
	}
	if !maps.Equal(sliceStoredVectors(t, slicedRoot), sliceStoredVectors(t, wholeRoot)) {
		t.Fatal("the slices stored rows or vectors that differ from one whole upsert")
	}
}

// TestUpsertConversationSliceBoundsOneSplitMessage delivers a message that requires
// several slices. Each slice respects the row limit plus one embedding request.
// The test verifies that the client stores every part of the message.
func TestUpsertConversationSliceBoundsOneSplitMessage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	doc := sliceDocument(0)
	doc.Text = strings.Repeat(sliceEditedFiller, sliceSplitRepeats)
	docs := []semsearch.SemDoc{doc}
	manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: sliceFingerprint}}
	client := openSliceClient(t, root, t.TempDir())
	defer func() { _ = client.Close(ctx) }()
	slicesRun := 0
	for {
		if slicesRun > sliceMessages {
			t.Fatal("the conversation did not finish in the expected slice count")
		}
		_, err := client.UpsertConversationSlice(ctx, sliceCollectionID, docs, manifest)
		slicesRun++
		if written := client.IngestStatus(sliceCollectionID).LastUpsert.RowsWritten; written > sliceRows+sliceRequestRows {
			t.Fatalf("One slice wrote %d rows; the test expected at most %d.", written, sliceRows+sliceRequestRows)
		}
		if err == nil {
			break
		}
		var partial *vectorsearch.PartialUpsertError
		if !errors.As(err, &partial) {
			t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
		}
	}
	if slicesRun < 3 {
		t.Fatalf("The message finished in %d slices; the test expected at least 3.", slicesRun)
	}
	stored := strings.Join(sliceStoredContents(t, root), "")
	if len(stored) != len(doc.Text) {
		t.Fatalf("The stored parts contain %d bytes; the test expected %d.", len(stored), len(doc.Text))
	}
}

// TestUpsertConversationSliceResumesWithoutARetainedPlan sets the memory limit below
// one plan's size. Each slice reads stored rows. The client embeds each vector once
// and stores the same rows and vectors as one whole upsert.
func TestUpsertConversationSliceResumesWithoutARetainedPlan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	docs := sliceDocuments(sliceMessages)
	client, err := vectorsearch.Open(ctx, vectorsearch.Options{
		Backend: vectorsearch.BackendLocal, LocalRoot: root, CheckpointDir: t.TempDir(), IngestSliceRows: sliceRows, IngestSlicePlanBytes: 1,
	})
	if err != nil {
		t.Fatalf("open the local client: %v", err)
	}
	rowsRead, embedded := runSlicesToCheckpoint(t, client, docs, sliceFingerprint)
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close the local client: %v", err)
	}
	if rowsRead <= sliceMessages {
		t.Fatalf("The slices read %d stored rows; the test expected more than %d without a retained plan.", rowsRead, sliceMessages)
	}
	if embedded != sliceMessages {
		t.Fatalf("The slices embedded %d vectors; the test expected %d.", embedded, sliceMessages)
	}
	wholeRoot := t.TempDir()
	whole := openSliceClient(t, wholeRoot, t.TempDir())
	manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: sliceFingerprint}}
	if _, err := whole.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest); err != nil {
		t.Fatalf("upsert the whole conversation: %v", err)
	}
	if err := whole.Close(ctx); err != nil {
		t.Fatalf("close the local client: %v", err)
	}
	if !maps.Equal(sliceStoredVectors(t, root), sliceStoredVectors(t, wholeRoot)) {
		t.Fatal("the slices stored rows or vectors that differ from one whole upsert")
	}
}

// TestUpsertConversationSliceResumesAfterPlanEviction sets a memory limit for one
// large plan. Planning a second partial conversation removes the first plan.
// The client then reads the first conversation's stored rows and finishes both
// conversations without embedding any vector twice.
func TestUpsertConversationSliceResumesAfterPlanEviction(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	client, err := vectorsearch.Open(ctx, vectorsearch.Options{
		Backend: vectorsearch.BackendLocal, LocalRoot: root, CheckpointDir: t.TempDir(), IngestSliceRows: sliceRows, IngestSlicePlanBytes: slicePlanFitsOne,
	})
	if err != nil {
		t.Fatalf("open the local client: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	firstDocs := sliceDocuments(sliceMessages)
	secondDocs := sliceDocuments(sliceMessages)
	for position := range secondDocs {
		secondDocs[position].ConversationID = sliceLaterSortedConversationID
		secondDocs[position].Text = "second " + secondDocs[position].Text
	}
	firstManifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: sliceFingerprint}}
	secondManifest := []semsearch.Fingerprint{{ConversationID: sliceLaterSortedConversationID, Value: sliceFingerprint}}
	var partial *vectorsearch.PartialUpsertError
	if _, err := client.UpsertConversationSlice(ctx, sliceCollectionID, firstDocs, firstManifest); !errors.As(err, &partial) {
		t.Fatalf("The first conversation's slice returned error %v; the test expected a partial upsert.", err)
	}
	embedded := client.IngestStatus(sliceCollectionID).LastUpsert.VectorsEmbedded
	if _, err := client.UpsertConversationSlice(ctx, sliceCollectionID, secondDocs, secondManifest); !errors.As(err, &partial) {
		t.Fatalf("The second conversation's slice returned error %v; the test expected a partial upsert.", err)
	}
	embedded += client.IngestStatus(sliceCollectionID).LastUpsert.VectorsEmbedded
	if _, err := client.UpsertConversationSlice(ctx, sliceCollectionID, firstDocs, firstManifest); !errors.As(err, &partial) {
		t.Fatalf("The resumed first conversation's slice returned error %v; the test expected a partial upsert.", err)
	}
	status := client.IngestStatus(sliceCollectionID).LastUpsert
	if status.RowsRead == 0 {
		t.Fatal("the client did not read stored rows after the second conversation should have displaced the first plan")
	}
	embedded += status.VectorsEmbedded
	_, firstEmbedded := runSlicesToCheckpoint(t, client, firstDocs, sliceFingerprint)
	_, secondEmbedded := runSlicesToCheckpointFor(t, client, secondDocs, secondManifest)
	embedded += firstEmbedded + secondEmbedded
	if embedded != 2*sliceMessages {
		t.Fatalf("The slices embedded %d vectors; the test expected %d.", embedded, 2*sliceMessages)
	}
	needed, err := client.SyncConversationManifest(ctx, sliceCollectionID, append(firstManifest, secondManifest...))
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed still contains %v after both conversations finished.", needed)
	}
}

// TestUpsertConversationSliceReplansChangedDocuments changes documents between slices
// without changing the fingerprint. After finishing the retained plan, the client
// reads stored rows and writes the new message.
func TestUpsertConversationSliceReplansChangedDocuments(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	client := openSliceClient(t, root, t.TempDir())
	manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: sliceFingerprint}}
	_, err := client.UpsertConversationSlice(ctx, sliceCollectionID, sliceDocuments(sliceMessages), manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The first slice returned error %v; the test expected a partial upsert.", err)
	}
	runSlicesToCheckpoint(t, client, sliceDocuments(sliceMessages+1), sliceFingerprint)
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close the local client: %v", err)
	}
	if rows := sliceStoredVectors(t, root); len(rows) != sliceMessages+1 {
		t.Fatalf("The final state has %d rows; the test expected %d.", len(rows), sliceMessages+1)
	}
}

// TestUpsertConversationSliceDetectsAnotherClient lets a second client finish a
// conversation between the first client's slices. The first client discards its
// plan and reads stored rows.
func TestUpsertConversationSliceDetectsAnotherClient(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	docs := sliceDocuments(sliceMessages)
	manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: sliceFingerprint}}
	first := openSliceClient(t, root, checkpoints)
	_, err := first.UpsertConversationSlice(ctx, sliceCollectionID, docs, manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The first slice returned error %v; the test expected a partial upsert.", err)
	}
	second := openSliceClient(t, root, checkpoints)
	if _, err := second.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest); err != nil {
		t.Fatalf("upsert with the second client: %v", err)
	}
	_, err = first.UpsertConversationSlice(ctx, sliceCollectionID, docs, manifest)
	if err != nil && !errors.As(err, &partial) {
		t.Fatalf("run a slice after the second client finished: %v", err)
	}
	if rowsRead := first.IngestStatus(sliceCollectionID).LastUpsert.RowsRead; rowsRead == 0 {
		t.Fatal("the first client kept its plan after the second client finished the conversation")
	}
}

// TestUpsertConversationSliceFollowsDeliveryOrder delivers a small conversation before
// a large conversation with an earlier sorted ID. The small conversation finishes.
// The large conversation still needs ingestion.
func TestUpsertConversationSliceFollowsDeliveryOrder(t *testing.T) {
	ctx := context.Background()
	client := openSliceClient(t, t.TempDir(), t.TempDir())
	defer func() { _ = client.Close(ctx) }()
	small := sliceDocument(0)
	small.ConversationID = sliceLaterSortedConversationID
	docs := append([]semsearch.SemDoc{small}, sliceDocuments(sliceMessages)...)
	manifest := []semsearch.Fingerprint{
		{ConversationID: sliceConversationID, Value: sliceFingerprint},
		{ConversationID: sliceLaterSortedConversationID, Value: sliceFingerprint},
	}
	_, err := client.UpsertConversationSlice(ctx, sliceCollectionID, docs, manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
	}
	if !slices.Equal(partial.Completed, []string{sliceLaterSortedConversationID}) {
		t.Fatalf("The upsert reports completed conversations %v; the test expected %s.", partial.Completed, sliceLaterSortedConversationID)
	}
	if !slices.Equal(partial.Incomplete, []string{sliceConversationID}) {
		t.Fatalf("The upsert reports incomplete conversations %v; the test expected %s.", partial.Incomplete, sliceConversationID)
	}
	needed, err := client.SyncConversationManifest(ctx, sliceCollectionID, manifest)
	if err != nil {
		t.Fatalf("list needed conversations: %v", err)
	}
	if !slices.Equal(needed, []string{sliceConversationID}) {
		t.Fatalf("needed contains %v; the test expected %s.", needed, sliceConversationID)
	}
}

// TestUpsertConversationSliceResumesAfterCanceledWrite cancels an upsert before its
// first embedding request. The client preserves the write record. A new client
// continues the write.
func TestUpsertConversationSliceResumesAfterCanceledWrite(t *testing.T) {
	root := t.TempDir()
	checkpoints := t.TempDir()
	docs := sliceDocuments(sliceRows / 2)
	ctx := context.Background()
	manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: sliceFingerprint}}
	client := openSliceClient(t, root, checkpoints)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := client.UpsertConversationSlice(canceled, sliceCollectionID, docs, manifest); err == nil {
		t.Fatal("the canceled slice returned success")
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("close the local client: %v", err)
	}
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, true)
	err, embedded := runSliceAfterRestart(t, root, checkpoints, docs, sliceFingerprint)
	if err != nil {
		t.Fatalf("resume the canceled slice: %v", err)
	}
	if embedded != len(docs) {
		t.Fatalf("The resumed slice embedded %d vectors; the test expected %d.", embedded, len(docs))
	}
	rows := sliceStoredVectors(t, root)
	if got := slices.Collect(maps.Keys(rows)); len(got) != len(docs) {
		t.Fatalf("The resumed slice stored %d rows; the test expected %d.", len(got), len(docs))
	}
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, false)
}
