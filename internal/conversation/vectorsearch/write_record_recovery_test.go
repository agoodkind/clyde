//go:build unix

package vectorsearch_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	recoveryWriteFolder       = ".in-progress"
	recoveryHeaderFile        = "collection.json"
	recoveryNameChars         = 32
	recoveryClaimWaitLimit    = time.Minute
	recoveryClaimPollDelay    = 10 * time.Millisecond
	recoveryFolderMode        = 0o700
	recoveryReadOnlyFolder    = 0o500
	recoveryRecordMode        = 0o600
	recoveryOlderRowID        = "chunk_older"
	recoveryOlderRowBytes     = 40
	recoveryProofConvID       = "codex:proof"
	recoveryWholeMessage      = 100_000
	recoveryAlternatePiece    = 15_000
	recoveryCodePathBytes     = 60_000
	recoveryMissingPieceBytes = 5_000
	recoveryCompletedEvent    = "conversation.vectorsearch.upsert_completed"
	recoveryOtherText         = "other conversation text"
	recoveryTimestamp         = int64(1_790_000_000)
	recoveryConversationCol   = "conversationId"
	recoveryTimestampCol      = "timestampUnix"
	// recoveryStoredProof is the expected family proof in the first encoding for the local
	// embedding model. Software updates must support stored records.
	recoveryStoredProof = "87e1f4e07330bb9a296883f6d3cde0f7e42267f753107f5b631d15a9c7b83f44"
)

func recoveryManifest(conversationID string) []semsearch.Fingerprint {
	return []semsearch.Fingerprint{{ConversationID: conversationID, Value: sliceFingerprint}}
}

func recoveryWriteFolderPath(checkpoints string) string {
	return filepath.Join(checkpoints, recoveryWriteFolder, vectorsearch.CollectionName(sliceCollectionID))
}

func recoveryFileStem(conversationID string) string {
	digest := sha256.Sum256([]byte(conversationID))
	return hex.EncodeToString(digest[:])[:recoveryNameChars]
}

func recoveryWriteRecords(t *testing.T, checkpoints string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(recoveryWriteFolderPath(checkpoints), "*.json"))
	if err != nil {
		t.Fatalf("The test could not list write records: %v.", err)
	}
	return paths
}

// leaveRecoveryWrite stores a checkpoint for the test conversation and then stops a later
// write with the same fingerprint. It returns the path of the write record.
func leaveRecoveryWrite(t *testing.T, root string, checkpoints string) string {
	t.Helper()
	ctx := context.Background()
	client := openSliceClient(t, root, checkpoints)
	manifest := recoveryManifest(sliceConversationID)
	if _, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, sliceDocuments(1), manifest); err != nil {
		t.Fatalf("The test could not write the first message: %v.", err)
	}
	_, err := client.UpsertConversationSlice(ctx, sliceCollectionID, sliceDocuments(sliceMessages), manifest)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("The test could not close the local client: %v.", err)
	}
	records := recoveryWriteRecords(t, checkpoints)
	if len(records) != 1 {
		t.Fatalf("The store contains write records %v; the test expected one record.", records)
	}
	return records[0]
}

// finishRecovery runs slices with one new client until the conversation has a checkpoint.
func finishRecovery(t *testing.T, root string, checkpoints string) {
	t.Helper()
	ctx := context.Background()
	client := openSliceClient(t, root, checkpoints)
	runSlicesToCheckpoint(t, client, sliceDocuments(sliceMessages), sliceFingerprint)
	if err := client.Close(ctx); err != nil {
		t.Fatalf("The test could not close the local client: %v.", err)
	}
}

// TestStoppedWriteWithSameFingerprintStaysNeededUntilRetry checks an interrupted forced
// backfill. The checkpoint has the fingerprint of the stopped write.
func TestStoppedWriteWithSameFingerprintStaysNeededUntilRetry(t *testing.T) {
	root := t.TempDir()
	checkpoints := t.TempDir()
	leaveRecoveryWrite(t, root, checkpoints)
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, true)
	finishRecovery(t, root, checkpoints)
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, false)
	if records := recoveryWriteRecords(t, checkpoints); len(records) != 0 {
		t.Fatalf("The store still contains write records %v after the retry.", records)
	}
}

// TestStoppedWriteInMemoryStaysNeededUntilRetry uses a client without a checkpoint
// folder. The same client keeps the stopped write record in memory.
func TestStoppedWriteInMemoryStaysNeededUntilRetry(t *testing.T) {
	ctx := context.Background()
	client := openSliceClient(t, t.TempDir(), "")
	defer func() { _ = client.Close(ctx) }()
	manifest := recoveryManifest(sliceConversationID)
	if _, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, sliceDocuments(1), manifest); err != nil {
		t.Fatalf("The test could not write the first message: %v.", err)
	}
	var partial *vectorsearch.PartialUpsertError
	if _, err := client.UpsertConversationSlice(ctx, sliceCollectionID, sliceDocuments(sliceMessages), manifest); !errors.As(err, &partial) {
		t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
	}
	needed, err := client.SyncConversationManifest(ctx, sliceCollectionID, manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if !slices.Equal(needed, []string{sliceConversationID}) {
		t.Fatalf("needed contains %v; the test expected %s.", needed, sliceConversationID)
	}
	runSlicesToCheckpoint(t, client, sliceDocuments(sliceMessages), sliceFingerprint)
	needed, err = client.SyncConversationManifest(ctx, sliceCollectionID, manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed still contains %v after the retry.", needed)
	}
}

// TestDamagedWriteRecordKeepsItsConversationNeeded checks invalid and unreadable records.
// The conversation still needs ingestion despite a matching checkpoint fingerprint. An
// upsert reports the error and keeps the stored rows. The unrelated conversation does not
// need ingestion.
func TestDamagedWriteRecordKeepsItsConversationNeeded(t *testing.T) {
	writeRecord := func(content string) func(t *testing.T, path string) {
		return func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(content), recoveryRecordMode); err != nil {
				t.Fatalf("The test could not write the record: %v.", err)
			}
		}
	}
	cases := map[string]func(t *testing.T, path string){
		"invalid JSON":                       writeRecord(`{"version":`),
		"valid JSON without required fields": writeRecord(`{"version":1}`),
		"another version":                    writeRecord(`{"version":999}`),
		"unreadable file": func(t *testing.T, path string) {
			t.Helper()
			if err := os.Chmod(path, 0o000); err != nil {
				t.Fatalf("The test could not make the record unreadable: %v.", err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(path, recoveryRecordMode); err != nil {
					t.Errorf("The test could not make the record readable: %v.", err)
				}
			})
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			checkpoints := t.TempDir()
			path := leaveRecoveryWrite(t, root, checkpoints)
			unrelated := sliceDocument(0)
			unrelated.ConversationID = sliceLaterSortedConversationID
			setup := openSliceClient(t, root, checkpoints)
			if _, err := setup.UpsertConversationDocuments(ctx, sliceCollectionID, []semsearch.SemDoc{unrelated}, recoveryManifest(sliceLaterSortedConversationID)); err != nil {
				t.Fatalf("The test could not write the unrelated conversation: %v.", err)
			}
			if err := setup.Close(ctx); err != nil {
				t.Fatalf("The test could not close the local client: %v.", err)
			}
			damage(t, path)
			before := sliceStoredVectors(t, root)

			client := openSliceClient(t, root, checkpoints)
			defer func() { _ = client.Close(ctx) }()
			manifest := append(recoveryManifest(sliceConversationID), recoveryManifest(sliceLaterSortedConversationID)...)
			needed, err := client.SyncConversationManifest(ctx, sliceCollectionID, manifest)
			if err != nil {
				t.Fatalf("The test could not list needed conversations: %v.", err)
			}
			if !slices.Equal(needed, []string{sliceConversationID}) {
				t.Fatalf("needed contains %v; the test expected only %s.", needed, sliceConversationID)
			}
			_, err = client.UpsertConversationDocuments(ctx, sliceCollectionID, sliceDocuments(sliceMessages), recoveryManifest(sliceConversationID))
			if !errors.Is(err, vectorsearch.ErrUnprovenStoppedWrite) {
				t.Fatalf("The upsert returned error %v; the test expected %v.", err, vectorsearch.ErrUnprovenStoppedWrite)
			}
			if !maps.Equal(sliceStoredVectors(t, root), before) {
				t.Fatal("The upsert changed stored rows despite the damaged write record.")
			}
		})
	}
}

// TestUnreadableWriteRecordFolderFailsManifest checks that an unreadable record folder
// makes the manifest sync fail despite a matching checkpoint fingerprint.
func TestUnreadableWriteRecordFolderFailsManifest(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	leaveRecoveryWrite(t, root, checkpoints)
	folder := recoveryWriteFolderPath(checkpoints)
	if err := os.Chmod(folder, 0o000); err != nil {
		t.Fatalf("The test could not make the record folder unreadable: %v.", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(folder, recoveryFolderMode); err != nil {
			t.Errorf("The test could not make the record folder readable: %v.", err)
		}
	})
	client := openSliceClient(t, root, checkpoints)
	defer func() { _ = client.Close(ctx) }()
	if _, err := client.SyncConversationManifest(ctx, sliceCollectionID, recoveryManifest(sliceConversationID)); err == nil {
		t.Fatal("The manifest sync succeeded with an unreadable record folder.")
	}
}

// recoveryHeaderGate uses a named pipe in place of the collection header. A new client
// waits while opening the collection until the test calls open.
type recoveryHeaderGate struct {
	headerPath string
	savedPath  string
	content    []byte
	pipe       *os.File
}

func closeRecoveryHeader(t *testing.T, root string) *recoveryHeaderGate {
	t.Helper()
	headerPath := filepath.Join(root, vectorsearch.CollectionName(sliceCollectionID), recoveryHeaderFile)
	content, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatalf("The test could not read the collection header: %v.", err)
	}
	savedPath := headerPath + ".saved"
	if err := os.Rename(headerPath, savedPath); err != nil {
		t.Fatalf("The test could not save the collection header: %v.", err)
	}
	if err := syscall.Mkfifo(headerPath, recoveryRecordMode); err != nil {
		t.Fatalf("The test could not create the named pipe: %v.", err)
	}
	pipe, err := os.OpenFile(headerPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("The test could not open the named pipe: %v.", err)
	}
	gate := &recoveryHeaderGate{headerPath: headerPath, savedPath: savedPath, content: content, pipe: pipe}
	t.Cleanup(func() { gate.open(t) })
	return gate
}

// open restores the header file and sends the header to a client that opened the pipe.
func (gate *recoveryHeaderGate) open(t *testing.T) {
	t.Helper()
	if gate.pipe == nil {
		return
	}
	pipe := gate.pipe
	gate.pipe = nil
	if err := os.Rename(gate.savedPath, gate.headerPath); err != nil {
		t.Errorf("The test could not restore the collection header: %v.", err)
	}
	if _, err := pipe.Write(gate.content); err != nil {
		t.Errorf("The test could not send the collection header: %v.", err)
	}
	if err := pipe.Close(); err != nil {
		t.Errorf("The test could not close the named pipe: %v.", err)
	}
}

func waitForRecoveryClaim(t *testing.T, checkpoints string, conversationID string) {
	t.Helper()
	claimPath := filepath.Join(recoveryWriteFolderPath(checkpoints), recoveryFileStem(conversationID)+".lock")
	deadline := time.Now().Add(recoveryClaimWaitLimit)
	for {
		if _, err := os.Stat(claimPath); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("The upsert did not claim the conversation before it read the collection.")
		}
		time.Sleep(recoveryClaimPollDelay)
	}
}

// TestConversationClaimAllowsOneClient pauses one client while it reads stored rows. That
// client already owns the claim. The second client does not treat the running write as
// stopped and cannot write the conversation.
func TestConversationClaimAllowsOneClient(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	leaveRecoveryWrite(t, root, checkpoints)
	docs := sliceDocuments(sliceMessages)
	manifest := recoveryManifest(sliceConversationID)
	gate := closeRecoveryHeader(t, root)

	first := openSliceClient(t, root, checkpoints)
	defer func() { _ = first.Close(ctx) }()
	finished := make(chan error, 1)
	go func() {
		_, err := first.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest)
		finished <- err
	}()
	waitForRecoveryClaim(t, checkpoints, sliceConversationID)

	second := openSliceClient(t, root, checkpoints)
	defer func() { _ = second.Close(ctx) }()
	needed, err := second.SyncConversationManifest(ctx, sliceCollectionID, manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed contains %v while another client runs the write.", needed)
	}
	var secondLog bytes.Buffer
	previousLogger := slog.Default()
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(&secondLog, nil)))
	_, err = second.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest)
	slog.SetDefault(previousLogger)
	log.SetOutput(previousWriter)
	log.SetFlags(previousFlags)
	if !errors.Is(err, vectorsearch.ErrConversationWriteBusy) {
		t.Fatalf("The second client returned error %v; the test expected %v.", err, vectorsearch.ErrConversationWriteBusy)
	}
	if strings.Contains(secondLog.String(), recoveryCompletedEvent) {
		t.Fatalf("The busy upsert logged %s: %s.", recoveryCompletedEvent, secondLog.String())
	}
	if records := recoveryWriteRecords(t, checkpoints); len(records) != 1 {
		t.Fatalf("The second client changed the write records: %v.", records)
	}

	gate.open(t)
	if err := <-finished; err != nil {
		t.Fatalf("The test could not finish the first client's upsert: %v.", err)
	}
	if records := recoveryWriteRecords(t, checkpoints); len(records) != 0 {
		t.Fatalf("The store still contains write records %v after the first client finished.", records)
	}
	if rows := sliceStoredVectors(t, root); len(rows) != sliceMessages {
		t.Fatalf("The store contains %d rows; the test expected %d.", len(rows), sliceMessages)
	}
}

func blockRecoveryCheckpoint(t *testing.T, checkpoints string, conversationID string) string {
	t.Helper()
	blocker := filepath.Join(checkpoints, vectorsearch.CollectionName(sliceCollectionID), recoveryFileStem(conversationID)+".json")
	if err := os.MkdirAll(filepath.Join(blocker, "blocker"), recoveryFolderMode); err != nil {
		t.Fatalf("The test could not create the checkpoint blocker: %v.", err)
	}
	return blocker
}

// TestFinishedWriteRemovalFailureRetriesAndCompacts stores a checkpoint while the record
// folder is read-only. The checkpoint lists the finished write. A later manifest sync
// removes the record and its write ID.
func TestFinishedWriteRemovalFailureRetriesAndCompacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	docs := sliceDocuments(1)
	manifest := recoveryManifest(sliceConversationID)
	blocker := blockRecoveryCheckpoint(t, checkpoints, sliceConversationID)
	client := openSliceClient(t, root, checkpoints)
	if _, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest); err == nil {
		t.Fatal("The upsert returned success despite the checkpoint blocker.")
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("The test could not close the local client: %v.", err)
	}
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatalf("The test could not remove the checkpoint blocker: %v.", err)
	}
	records := recoveryWriteRecords(t, checkpoints)
	if len(records) != 1 {
		t.Fatalf("The store contains write records %v; the test expected one record.", records)
	}

	gate := closeRecoveryHeader(t, root)
	retry := openSliceClient(t, root, checkpoints)
	defer func() { _ = retry.Close(ctx) }()
	finished := make(chan error, 1)
	go func() {
		_, err := retry.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest)
		finished <- err
	}()
	waitForRecoveryClaim(t, checkpoints, sliceConversationID)
	folder := recoveryWriteFolderPath(checkpoints)
	if err := os.Chmod(folder, recoveryReadOnlyFolder); err != nil {
		t.Fatalf("The test could not make the record folder read-only: %v.", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(folder, recoveryFolderMode); err != nil {
			t.Errorf("The test could not make the record folder writable: %v.", err)
		}
	})
	gate.open(t)
	if err := <-finished; err != nil {
		t.Fatalf("The test could not retry the upsert: %v.", err)
	}
	if _, err := os.Stat(records[0]); err != nil {
		t.Fatalf("The test could not inspect the record in the read-only folder: %v.", err)
	}
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, false)

	if err := os.Chmod(folder, recoveryFolderMode); err != nil {
		t.Fatalf("The test could not make the record folder writable: %v.", err)
	}
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, false)
	if _, err := os.Stat(records[0]); !os.IsNotExist(err) {
		t.Fatalf("The manifest sync did not retry the removal: %v.", err)
	}
	checkpointPath := filepath.Join(checkpoints, vectorsearch.CollectionName(sliceCollectionID), recoveryFileStem(sliceConversationID)+".json")
	content, err := os.ReadFile(checkpointPath)
	if err != nil {
		t.Fatalf("The test could not read the checkpoint: %v.", err)
	}
	var checkpoint struct {
		Fingerprint     string   `json:"fingerprint"`
		CommittedWrites []string `json:"committed_writes"`
	}
	if err := json.Unmarshal(content, &checkpoint); err != nil {
		t.Fatalf("The test could not decode the checkpoint: %v.", err)
	}
	if checkpoint.Fingerprint != sliceFingerprint || len(checkpoint.CommittedWrites) != 0 {
		t.Fatalf("The checkpoint stores %+v after the removal.", checkpoint)
	}
}

func openRecoveryStore(t *testing.T, root string) *codestore.Store {
	t.Helper()
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("The test could not open the local store: %v.", err)
	}
	return store
}

func recoveryStoredRow(t *testing.T, id string, path string, content string, splitPart int32, recorded bool) collection.Row {
	t.Helper()
	model, err := staticembed.Load()
	if err != nil {
		t.Fatalf("The test could not load the embedding model: %v.", err)
	}
	return collection.Row{
		ID: id, Content: content, RelativePath: path, Metadata: "{}",
		SplitPart: splitPart, SplitPartRecorded: recorded, Vector: model.Vector(content),
		Scalars: map[string]collection.ScalarValue{
			recoveryConversationCol: collection.StringScalar(sliceConversationID),
			recoveryTimestampCol:    collection.Int64Scalar(recoveryTimestamp),
		},
	}
}

// TestUpsertReportsStoppedWriteWithOlderRowsAndContinues checks a stopped write for a
// family with an older row. The upsert cannot establish which text is missing. It
// preserves the write record and finishes the other conversation.
func TestUpsertReportsStoppedWriteWithOlderRowsAndContinues(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	stuck := sliceDocument(0)
	stuck.Text = strings.Repeat(sliceEditedFiller, sliceSplitRepeats)
	other := sliceDocument(0)
	other.ConversationID = sliceLaterSortedConversationID
	other.Text = recoveryOtherText
	first := openSliceClient(t, root, checkpoints)
	var partial *vectorsearch.PartialUpsertError
	if _, err := first.UpsertConversationSlice(ctx, sliceCollectionID, []semsearch.SemDoc{stuck}, recoveryManifest(sliceConversationID)); !errors.As(err, &partial) {
		t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("The test could not close the local client: %v.", err)
	}
	records := recoveryWriteRecords(t, checkpoints)
	if len(records) != 1 {
		t.Fatalf("The store contains write records %v; the test expected one record.", records)
	}

	// The older row does not record a split part. It stores the start of the message.
	store := openRecoveryStore(t, root)
	collectionName := vectorsearch.CollectionName(sliceCollectionID)
	stored, err := store.QueryRows(ctx, collection.RowsRequest{
		Collection: collectionName, Declaration: vectorsearch.Declaration(),
		ItemIDs: nil, PathPrefixes: []string{"conv/"}, IncludeVector: false,
	})
	if err != nil {
		t.Fatalf("The test could not read stored rows: %v.", err)
	}
	older := recoveryStoredRow(t, recoveryOlderRowID, stored[0].RelativePath, stuck.Text[:recoveryOlderRowBytes], 0, false)
	if err := store.Upsert(ctx, collectionName, vectorsearch.Declaration(), []collection.Row{older}); err != nil {
		t.Fatalf("The test could not write the older row: %v.", err)
	}
	store.Close()
	before := sliceStoredVectors(t, root)

	retry := openSliceClient(t, root, checkpoints)
	defer func() { _ = retry.Close(ctx) }()
	manifest := append(recoveryManifest(sliceConversationID), recoveryManifest(sliceLaterSortedConversationID)...)
	_, err = retry.UpsertConversationDocuments(ctx, sliceCollectionID, []semsearch.SemDoc{stuck, other}, manifest)
	if !errors.Is(err, vectorsearch.ErrUnprovenStoppedWrite) {
		t.Fatalf("The upsert returned error %v; the test expected %v.", err, vectorsearch.ErrUnprovenStoppedWrite)
	}
	needed, err := retry.SyncConversationManifest(ctx, sliceCollectionID, manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if !slices.Equal(needed, []string{sliceConversationID}) {
		t.Fatalf("needed contains %v; the test expected only %s.", needed, sliceConversationID)
	}
	if remaining := recoveryWriteRecords(t, checkpoints); !slices.Equal(remaining, records) {
		t.Fatalf("The store contains write records %v; the test expected %v.", remaining, records)
	}
	after := sliceStoredVectors(t, root)
	for id, vector := range before {
		if after[id] != vector {
			t.Fatalf("The upsert changed stored row %s.", id)
		}
	}
	if len(after) != len(before)+1 {
		t.Fatalf("The store contains %d rows; the test expected %d.", len(after), len(before)+1)
	}
}

func recoveryRowID(path string, splitPart int32, content string) string {
	digest := sha256.Sum256(fmt.Appendf(nil, "%s:0:0:%d:%s", path, splitPart, content))
	return "chunk_" + hex.EncodeToString(digest[:])[:16]
}

// writeRecoveryRecord stores a write record for family in the checkpoint store's format.
// The proof covers earlier text and does not verify the current text.
func writeRecoveryRecord(t *testing.T, checkpoints string, family string) {
	t.Helper()
	const writeID = "stopped"
	record, err := json.Marshal(map[string]any{
		"version": 1, "write_id": writeID, "conversation_id": sliceConversationID, "fingerprint": sliceFingerprint,
		"families": []map[string]string{{"family": family, "proof": "earlier-text"}},
	})
	if err != nil {
		t.Fatalf("The test could not encode the write record: %v.", err)
	}
	folder := recoveryWriteFolderPath(checkpoints)
	if err := os.MkdirAll(folder, recoveryFolderMode); err != nil {
		t.Fatalf("The test could not create the record folder: %v.", err)
	}
	if err := os.WriteFile(filepath.Join(folder, recoveryFileStem(sliceConversationID)+"."+writeID+".json"), record, recoveryRecordMode); err != nil {
		t.Fatalf("The test could not write the record: %v.", err)
	}
}

// TestRetryWritesMissingPiecesOnlyAfterStoppedWrite stores pieces of one message and runs
// an upsert. The upsert writes missing pieces only when a write record exists for the
// message's family and the stored pieces do not cover the full text.
func TestRetryWritesMissingPiecesOnlyAfterStoppedWrite(t *testing.T) {
	path := "conv/" + sliceConversationID + "/0"
	text := strings.Repeat("0123456789", recoveryMissingPieceBytes/10)
	budget := staticembed.PassageBytes
	piece := func(start int, end int) recoveryPiece {
		return recoveryPiece{start: start, end: end, id: "", splitPartMissing: false}
	}
	alternateLayout := []recoveryPiece{piece(0, budget-500), piece(budget-500, budget+500), piece(budget+500, len(text))}
	cases := []struct {
		name   string
		pieces []recoveryPiece
		family string
		want   map[int32]int
	}{
		{
			name:   "stopped write lacks later pieces",
			pieces: []recoveryPiece{piece(0, budget)},
			family: path,
			want:   map[int32]int{int32(budget + 1): budget, int32(2*budget + 1): len(text) - 2*budget},
		},
		{
			name:   "complete older shorter version",
			pieces: []recoveryPiece{piece(0, budget)},
			family: "",
			want:   map[int32]int{},
		},
		{
			name:   "complete alternate layout",
			pieces: alternateLayout,
			family: "",
			want:   map[int32]int{},
		},
		{
			name:   "complete alternate layout with a stopped write record",
			pieces: alternateLayout,
			family: path,
			want:   map[int32]int{},
		},
		{
			name:   "row without a recorded split part",
			pieces: []recoveryPiece{{start: 0, end: budget, id: "", splitPartMissing: true}},
			family: path,
			want:   map[int32]int{},
		},
		{
			name:   "row with an older key",
			pieces: []recoveryPiece{{start: 0, end: budget, id: recoveryOlderRowID, splitPartMissing: false}},
			family: path,
			want:   map[int32]int{},
		},
		{
			name:   "stopped write record for another family",
			pieces: []recoveryPiece{piece(0, budget)},
			family: "conv/" + sliceConversationID + "/1",
			want:   map[int32]int{},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			checkpoints := t.TempDir()
			rows := make([]collection.Row, 0, len(testCase.pieces))
			for _, stored := range testCase.pieces {
				rows = append(rows, stored.row(t, path, text))
			}
			writeRecoveryRows(t, root, rows)
			if testCase.family != "" {
				writeRecoveryRecord(t, checkpoints, testCase.family)
			}
			before := sliceStoredVectors(t, root)
			doc := sliceDocument(0)
			doc.Text = text
			client := openSliceClient(t, root, checkpoints)
			defer func() { _ = client.Close(ctx) }()
			if _, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, []semsearch.SemDoc{doc}, recoveryManifest(sliceConversationID)); err != nil {
				t.Fatalf("The test could not run the upsert: %v.", err)
			}
			got := make(map[int32]int)
			for _, row := range readRecoveryRows(t, root) {
				if _, stored := before[row.ID]; !stored {
					got[row.SplitPart] = len(row.Content)
				}
			}
			if !maps.Equal(got, testCase.want) {
				t.Fatalf("The upsert wrote pieces %v; the test expected %v.", got, testCase.want)
			}
		})
	}
}

// recoveryPiece specifies a stored row with content text[start:end]. A nonempty ID
// replaces the row ID that current software generates.
type recoveryPiece struct {
	start            int
	end              int
	id               string
	splitPartMissing bool
}

func (piece recoveryPiece) row(t *testing.T, path string, text string) collection.Row {
	t.Helper()
	content := text[piece.start:piece.end]
	splitPart := int32(piece.start + 1)
	id := piece.id
	if id == "" {
		id = recoveryRowID(path, splitPart, content)
	}
	return recoveryStoredRow(t, id, path, content, splitPart, !piece.splitPartMissing)
}

func writeRecoveryRows(t *testing.T, root string, rows []collection.Row) {
	t.Helper()
	ctx := context.Background()
	store := openRecoveryStore(t, root)
	defer store.Close()
	collectionName := vectorsearch.CollectionName(sliceCollectionID)
	ensure := collection.EnsureRequest{Collection: collectionName, Declaration: vectorsearch.Declaration(), Dimension: staticembed.Dimensions}
	if err := store.EnsureCollection(ctx, ensure); err != nil {
		t.Fatalf("The test could not create the collection: %v.", err)
	}
	if err := store.Upsert(ctx, collectionName, vectorsearch.Declaration(), rows); err != nil {
		t.Fatalf("The test could not write the stored rows: %v.", err)
	}
}

func readRecoveryRows(t *testing.T, root string) []collection.StoredRow {
	t.Helper()
	store := openRecoveryStore(t, root)
	defer store.Close()
	rows, err := store.QueryRows(context.Background(), collection.RowsRequest{
		Collection: vectorsearch.CollectionName(sliceCollectionID), Declaration: vectorsearch.Declaration(),
		ItemIDs: nil, PathPrefixes: []string{"conv/"}, IncludeVector: false,
	})
	if err != nil {
		t.Fatalf("The test could not read stored rows: %v.", err)
	}
	return rows
}

// TestChangedFamilyKeepsItsRecordWithoutStayingNeeded stops a write and then changes its
// text. The store retains the record and does not mark the conversation for ingestion.
// When the earlier text returns, the upsert uses the record and removes it.
func TestChangedFamilyKeepsItsRecordWithoutStayingNeeded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	original := sliceDocument(0)
	original.Text = strings.Repeat(sliceEditedFiller, sliceSplitRepeats)
	edited := original
	edited.Text = original.Text + sliceEditedEnd

	err, _ := runSliceAfterRestart(t, root, checkpoints, []semsearch.SemDoc{original}, sliceFingerprint)
	var partial *vectorsearch.PartialUpsertError
	if !errors.As(err, &partial) {
		t.Fatalf("The slice returned error %v; the test expected a partial upsert.", err)
	}
	upsertRecovery := func(doc semsearch.SemDoc, fingerprint string) {
		t.Helper()
		client := openSliceClient(t, root, checkpoints)
		manifest := []semsearch.Fingerprint{{ConversationID: sliceConversationID, Value: fingerprint}}
		if _, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, []semsearch.SemDoc{doc}, manifest); err != nil {
			t.Fatalf("The test could not run the upsert: %v.", err)
		}
		if err := client.Close(ctx); err != nil {
			t.Fatalf("The test could not close the local client: %v.", err)
		}
	}
	upsertRecovery(edited, sliceChangedPrint)
	requireSliceNeeded(t, root, checkpoints, sliceChangedPrint, false)
	records := recoveryWriteRecords(t, checkpoints)
	if len(records) != 1 {
		t.Fatalf("The store contains write records %v; the test expected one record.", records)
	}
	content, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatalf("The test could not read the write record: %v.", err)
	}
	var record struct {
		Retained bool `json:"retained"`
	}
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("The test could not decode the write record: %v.", err)
	}
	if !record.Retained {
		t.Fatal("The store did not retain the write record of the changed family.")
	}
	before := sliceStoredVectors(t, root)

	upsertRecovery(original, sliceFingerprint)
	requireSliceNeeded(t, root, checkpoints, sliceFingerprint, false)
	if remaining := recoveryWriteRecords(t, checkpoints); len(remaining) != 0 {
		t.Fatalf("The store still contains write records %v after the earlier text returned.", remaining)
	}
	after := sliceStoredVectors(t, root)
	for id, vector := range before {
		if after[id] != vector {
			t.Fatalf("The upsert changed stored row %s.", id)
		}
	}
	contents := sliceStoredContents(t, root)
	if !slices.ContainsFunc(contents, func(stored string) bool { return strings.HasSuffix(stored, sliceEditedEnd) }) {
		t.Fatal("The upsert removed the end of the edited message.")
	}
	if !slices.ContainsFunc(contents, func(stored string) bool {
		return strings.HasSuffix(stored, sliceEditedFiller) && strings.HasSuffix(original.Text, stored)
	}) {
		t.Fatal("The stored rows do not include the end of the earlier message.")
	}
}

// TestOverlappingWritesOnOneInMemoryClient runs two upserts on a client without a
// checkpoint folder. The first upsert to claim the conversation waits at the collection
// header. The other upsert returns a busy error, and the first upsert then finishes.
func TestOverlappingWritesOnOneInMemoryClient(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	other := sliceDocument(0)
	other.ConversationID = sliceLaterSortedConversationID
	setup := openSliceClient(t, root, t.TempDir())
	if _, err := setup.UpsertConversationDocuments(ctx, sliceCollectionID, []semsearch.SemDoc{other}, recoveryManifest(sliceLaterSortedConversationID)); err != nil {
		t.Fatalf("The test could not create the collection: %v.", err)
	}
	if err := setup.Close(ctx); err != nil {
		t.Fatalf("The test could not close the local client: %v.", err)
	}
	gate := closeRecoveryHeader(t, root)
	client := openSliceClient(t, root, "")
	defer func() { _ = client.Close(ctx) }()
	docs := sliceDocuments(1)
	manifest := recoveryManifest(sliceConversationID)
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, docs, manifest)
			results <- err
		}()
	}
	select {
	case err := <-results:
		if !errors.Is(err, vectorsearch.ErrConversationWriteBusy) {
			t.Fatalf("The overlapping upsert returned error %v; the test expected %v.", err, vectorsearch.ErrConversationWriteBusy)
		}
	case <-time.After(recoveryClaimWaitLimit):
		t.Fatal("The upserts did not return a busy error before the time limit.")
	}
	gate.open(t)
	if err := <-results; err != nil {
		t.Fatalf("The test could not finish the first upsert: %v.", err)
	}
	needed, err := client.SyncConversationManifest(ctx, sliceCollectionID, manifest)
	if err != nil {
		t.Fatalf("The test could not list needed conversations: %v.", err)
	}
	if len(needed) != 0 {
		t.Fatalf("needed still contains %v after the upsert finished.", needed)
	}
}

// TestRetryWritesOnlyIncompletePaths checks a message over 60,000 bytes. Stored pieces
// cover its first path with different split boundaries. A stopped write stored only the
// first piece of its second path. The retry writes only the missing pieces.
func TestRetryWritesOnlyIncompletePaths(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	checkpoints := t.TempDir()
	doc := sliceDocument(0)
	doc.Text = strings.Repeat("0123456789", recoveryWholeMessage/10)
	family := "conv/" + sliceConversationID + "/0"
	firstPath := family + "/0"
	secondPath := family + "/1"
	firstText := doc.Text[:recoveryCodePathBytes]
	secondText := doc.Text[recoveryCodePathBytes:]

	rows := make([]collection.Row, 0)
	for start := 0; start < len(firstText); start += recoveryAlternatePiece {
		content := firstText[start:min(start+recoveryAlternatePiece, len(firstText))]
		splitPart := int32(start + 1)
		rows = append(rows, recoveryStoredRow(t, recoveryRowID(firstPath, splitPart, content), firstPath, content, splitPart, true))
	}
	secondHead := secondText[:staticembed.PassageBytes]
	rows = append(rows, recoveryStoredRow(t, recoveryRowID(secondPath, 1, secondHead), secondPath, secondHead, 1, true))
	writeRecoveryRows(t, root, rows)
	before := sliceStoredVectors(t, root)

	writeRecoveryRecord(t, checkpoints, family)

	client := openSliceClient(t, root, checkpoints)
	defer func() { _ = client.Close(ctx) }()
	if _, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, []semsearch.SemDoc{doc}, recoveryManifest(sliceConversationID)); err != nil {
		t.Fatalf("The test could not retry the conversation: %v.", err)
	}
	firstPathRows := 0
	offsets := make(map[int32]string)
	for _, row := range readRecoveryRows(t, root) {
		switch row.RelativePath {
		case firstPath:
			firstPathRows++
		case secondPath:
			offsets[row.SplitPart] = row.Content
		default:
			t.Fatalf("The retry stored unexpected path %s.", row.RelativePath)
		}
	}
	if firstPathRows != len(rows)-1 {
		t.Fatalf("The first path has %d rows; the test expected %d.", firstPathRows, len(rows)-1)
	}
	rebuilt := ""
	for _, splitPart := range slices.Sorted(maps.Keys(offsets)) {
		rebuilt += offsets[splitPart]
	}
	if rebuilt != secondText {
		t.Fatalf("The second path stores %d bytes; the test expected %d.", len(rebuilt), len(secondText))
	}
	current := sliceStoredVectors(t, root)
	for id, vector := range before {
		if current[id] != vector {
			t.Fatalf("The retry changed stored row %s.", id)
		}
	}
}

func recoveryProofDocument() semsearch.SemDoc {
	return semsearch.SemDoc{
		ConversationID: recoveryProofConvID, ParentConversationID: "codex:parent", MessageIndex: 3, Role: "user",
		TimestampUnix: recoveryTimestamp, Text: "message text", Tools: nil, Thinking: "",
		WorkspaceRoot: "/work", Archived: false, LoadRules: "rules",
	}
}

// recoveryRecordedProof runs an upsert that stops before its checkpoint and returns the
// family proof in the write record.
func recoveryRecordedProof(t *testing.T, doc semsearch.SemDoc) string {
	t.Helper()
	ctx := context.Background()
	checkpoints := t.TempDir()
	blockRecoveryCheckpoint(t, checkpoints, recoveryProofConvID)
	client := openSliceClient(t, t.TempDir(), checkpoints)
	defer func() { _ = client.Close(ctx) }()
	if _, err := client.UpsertConversationDocuments(ctx, sliceCollectionID, []semsearch.SemDoc{doc}, recoveryManifest(recoveryProofConvID)); err == nil {
		t.Fatal("The upsert returned success despite the checkpoint blocker.")
	}
	records := recoveryWriteRecords(t, checkpoints)
	if len(records) != 1 {
		t.Fatalf("The store contains write records %v; the test expected one record.", records)
	}
	content, err := os.ReadFile(records[0])
	if err != nil {
		t.Fatalf("The test could not read the write record: %v.", err)
	}
	var record struct {
		Families []struct {
			Proof string `json:"proof"`
		} `json:"families"`
	}
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("The test could not decode the write record: %v.", err)
	}
	if len(record.Families) != 1 {
		t.Fatalf("The write record lists %d families; the test expected one.", len(record.Families))
	}
	return record.Families[0].Proof
}

func TestWriteRecordProofKeepsItsStoredEncoding(t *testing.T) {
	if proof := recoveryRecordedProof(t, recoveryProofDocument()); proof != recoveryStoredProof {
		t.Fatalf("The write record has proof %s; the test expected %s.", proof, recoveryStoredProof)
	}
}

func TestWriteRecordProofCoversProjectedFields(t *testing.T) {
	base := recoveryRecordedProof(t, recoveryProofDocument())
	changes := map[string]func(*semsearch.SemDoc){
		"text":      func(doc *semsearch.SemDoc) { doc.Text = "other text" },
		"parent":    func(doc *semsearch.SemDoc) { doc.ParentConversationID = "codex:other" },
		"role":      func(doc *semsearch.SemDoc) { doc.Role = "assistant" },
		"timestamp": func(doc *semsearch.SemDoc) { doc.TimestampUnix++ },
		"workspace": func(doc *semsearch.SemDoc) { doc.WorkspaceRoot = "/other" },
		"archived":  func(doc *semsearch.SemDoc) { doc.Archived = true },
		"rules":     func(doc *semsearch.SemDoc) { doc.LoadRules = "other" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			doc := recoveryProofDocument()
			change(&doc)
			if proof := recoveryRecordedProof(t, doc); proof == base {
				t.Fatal("The proof did not change.")
			}
		})
	}
}
