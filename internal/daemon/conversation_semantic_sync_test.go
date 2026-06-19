package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
)

type semanticSyncCall struct {
	CollectionID string
	Manifest     []semsearch.Fingerprint
}

type semanticUpsertCall struct {
	CollectionID string
	Docs         []semsearch.SemDoc
	Manifest     []semsearch.Fingerprint
}

type fakeConversationSemanticClient struct {
	syncCalls   []semanticSyncCall
	upsertCalls []semanticUpsertCall
	needed      []string
	upsertErr   error
	// jobStates maps a job id to the state JobState reports; a missing entry
	// reports "completed" so passes are never skipped unless a test asks.
	jobStates     map[string]string
	jobStateCalls []string
}

func (c *fakeConversationSemanticClient) JobState(_ context.Context, jobID string) (string, error) {
	c.jobStateCalls = append(c.jobStateCalls, jobID)
	if state, found := c.jobStates[jobID]; found {
		return state, nil
	}
	return "completed", nil
}

func (c *fakeConversationSemanticClient) SyncConversationManifest(_ context.Context, collectionID string, manifest []semsearch.Fingerprint) ([]string, error) {
	c.syncCalls = append(c.syncCalls, semanticSyncCall{
		CollectionID: collectionID,
		Manifest:     append([]semsearch.Fingerprint(nil), manifest...),
	})
	return append([]string(nil), c.needed...), nil
}

func (c *fakeConversationSemanticClient) UpsertConversationDocuments(_ context.Context, collectionID string, docs []semsearch.SemDoc, manifest []semsearch.Fingerprint) (string, error) {
	c.upsertCalls = append(c.upsertCalls, semanticUpsertCall{
		CollectionID: collectionID,
		Docs:         append([]semsearch.SemDoc(nil), docs...),
		Manifest:     append([]semsearch.Fingerprint(nil), manifest...),
	})
	if c.upsertErr != nil {
		return "", c.upsertErr
	}
	return "upsert-job", nil
}

type fakeConversationSemanticIndex struct {
	records      []conversation.StampedRecord
	messagesByID map[string][]transcript.Message
	loadOptions  []conversation.LoadOptions
}

func (idx *fakeConversationSemanticIndex) ListWithStamps(_ context.Context) ([]conversation.StampedRecord, error) {
	return append([]conversation.StampedRecord(nil), idx.records...), nil
}

func (idx *fakeConversationSemanticIndex) LoadMessagesWithOptions(record conversation.Record, opts conversation.LoadOptions) ([]transcript.Message, error) {
	idx.loadOptions = append(idx.loadOptions, opts)
	messages, ok := idx.messagesByID[record.ID]
	if !ok {
		return nil, fmt.Errorf("messages for %s not found", record.ID)
	}
	return append([]transcript.Message(nil), messages...), nil
}

// TestConversationSemanticSyncSendsNeededConversations proves the two-call
// feeder: the worker states the full manifest with one fingerprint per
// conversation, and on the engine reporting one conversation needed, sends that
// conversation's documents plus the full manifest, with the semantic projection
// flags off and the manifest fingerprint derived from the file stamp.
func TestConversationSemanticSyncSendsNeededConversations(t *testing.T) {
	conversationID := "codex:one"
	newStamp := semanticTestStamp(20, 200)
	record := semanticTestRecord(conversationID)
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: record, Stamp: newStamp}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first"},
				{Role: "assistant", Timestamp: time.Unix(1710000030, 0), Text: "second"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.syncCalls) != 1 {
		t.Fatalf("sync calls = %d, want 1", len(client.syncCalls))
	}
	manifest := client.syncCalls[0].Manifest
	if len(manifest) != 1 {
		t.Fatalf("manifest size = %d, want 1", len(manifest))
	}
	if manifest[0].ConversationID != conversationID {
		t.Fatalf("manifest id = %q, want %q", manifest[0].ConversationID, conversationID)
	}
	if manifest[0].Value != newStamp.Fingerprint() {
		t.Fatalf("manifest fingerprint = %q, want %q", manifest[0].Value, newStamp.Fingerprint())
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1", len(client.upsertCalls))
	}
	call := client.upsertCalls[0]
	if call.CollectionID != "collection-test" {
		t.Fatalf("collection id = %q, want collection-test", call.CollectionID)
	}
	if len(call.Manifest) != 1 {
		t.Fatalf("upsert manifest size = %d, want 1", len(call.Manifest))
	}
	if len(call.Docs) != 2 {
		t.Fatalf("upsert docs = %d, want 2", len(call.Docs))
	}
	if call.Docs[0].MessageIndex != 0 || call.Docs[1].MessageIndex != 1 {
		t.Fatalf("message indices = %d, %d; want 0, 1", call.Docs[0].MessageIndex, call.Docs[1].MessageIndex)
	}
	if call.Docs[0].Role != "user" || call.Docs[1].Role != "assistant" {
		t.Fatalf("roles = %q, %q; want user, assistant", call.Docs[0].Role, call.Docs[1].Role)
	}
	if call.Docs[0].Text != "first" || call.Docs[1].Text != "second" {
		t.Fatalf("texts = %q, %q; want first, second", call.Docs[0].Text, call.Docs[1].Text)
	}
	if len(index.loadOptions) != 1 {
		t.Fatalf("load options calls = %d, want 1", len(index.loadOptions))
	}
	opts := index.loadOptions[0]
	if opts.IncludeSystemPrompts || opts.IncludeSystemMessages || opts.IncludeToolOutputs {
		t.Fatalf("load options = %+v, want all semantic projection flags false", opts)
	}
}

// TestConversationSemanticSyncSendsNothingWhenEngineNeedsNothing proves the
// worker loads no transcripts and sends no documents when the engine reports
// every conversation already up to date. A re-run with no changes is inert.
func TestConversationSemanticSyncSendsNothingWhenEngineNeedsNothing(t *testing.T) {
	conversationID := "codex:stable"
	stamp := semanticTestStamp(30, 300)
	index := &fakeConversationSemanticIndex{
		records:      []conversation.StampedRecord{{Record: semanticTestRecord(conversationID), Stamp: stamp}},
		messagesByID: map[string][]transcript.Message{},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: nil}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.syncCalls) != 1 {
		t.Fatalf("sync calls = %d, want 1", len(client.syncCalls))
	}
	if len(client.upsertCalls) != 0 {
		t.Fatalf("upsert calls = %d, want 0", len(client.upsertCalls))
	}
	if len(index.loadOptions) != 0 {
		t.Fatalf("load calls = %d, want 0", len(index.loadOptions))
	}
}

// TestConversationSemanticSyncManifestReflectsCurrentRecordsOnly proves the
// manifest the worker states each pass is exactly the current conversation set:
// a removed transcript is absent from the manifest, which is how the engine
// drops it. The worker holds no change-tracking state and issues no delete.
func TestConversationSemanticSyncManifestReflectsCurrentRecordsOnly(t *testing.T) {
	presentID := "codex:present"
	presentStamp := semanticTestStamp(40, 400)
	index := &fakeConversationSemanticIndex{
		records:      []conversation.StampedRecord{{Record: semanticTestRecord(presentID), Stamp: presentStamp}},
		messagesByID: map[string][]transcript.Message{},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: nil}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.syncCalls) != 1 {
		t.Fatalf("sync calls = %d, want 1", len(client.syncCalls))
	}
	manifest := client.syncCalls[0].Manifest
	if len(manifest) != 1 || manifest[0].ConversationID != presentID {
		t.Fatalf("manifest = %+v, want only %q", manifest, presentID)
	}
}

// TestConversationSemanticSyncBatchesNeededConversations proves the worker sends
// every needed conversation's documents in one upsert call alongside the full
// manifest.
func TestConversationSemanticSyncBatchesNeededConversations(t *testing.T) {
	firstID := "codex:one"
	secondID := "claude:two"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{
			{Record: semanticTestRecord(firstID), Stamp: semanticTestStamp(20, 200)},
			{Record: semanticTestRecord(secondID), Stamp: semanticTestStamp(21, 210)},
		},
		messagesByID: map[string][]transcript.Message{
			firstID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first-a"},
			},
			secondID: {
				{Role: "user", Timestamp: time.Unix(1710000100, 0), Text: "second-a"},
				{Role: "assistant", Timestamp: time.Unix(1710000130, 0), Text: "second-b"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{firstID, secondID}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1 (single batched call)", len(client.upsertCalls))
	}
	call := client.upsertCalls[0]
	if len(call.Docs) != 3 {
		t.Fatalf("batched docs = %d, want 3 across both conversations", len(call.Docs))
	}
	if len(call.Manifest) != 2 {
		t.Fatalf("upsert manifest size = %d, want 2", len(call.Manifest))
	}
	docConversationIDs := map[string]int{}
	for _, doc := range call.Docs {
		docConversationIDs[doc.ConversationID]++
	}
	if docConversationIDs[firstID] != 1 || docConversationIDs[secondID] != 2 {
		t.Fatalf("docs per conversation = %v, want %s:1 %s:2", docConversationIDs, firstID, secondID)
	}
}

// TestConversationSemanticSyncEngineBusyIsBenign proves a conflicting-active-job
// rejection from a still-running engine job does not fail the pass: the worker
// attempted the upsert and returns no error, so the next pass retries.
func TestConversationSemanticSyncEngineBusyIsBenign(t *testing.T) {
	conversationID := "codex:one"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: semanticTestRecord(conversationID), Stamp: semanticTestStamp(20, 200)}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{
		needed:    []string{conversationID},
		upsertErr: errors.New(`upsert semantic conversation documents for collection "collection-test": conflicting active job j1 for conversation collection chat:///collection-test`),
	}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error on a busy engine, want nil: %v", err)
	}
	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert attempts = %d, want 1", len(client.upsertCalls))
	}
}

func TestConversationSemanticSyncCarriesForkParentIntoDocs(t *testing.T) {
	conversationID := "codex:fork-child"
	stamp := semanticTestStamp(20, 200)
	record := semanticTestRecord(conversationID)
	record.Lineage = &conversation.Lineage{
		Kind:              conversation.ConversationLineageKindFork,
		ParentProvider:    conversation.ProviderCodex,
		ParentNativeID:    "parent-thread",
		ParentMessageUUID: "msg",
	}
	wantParentID, ok := conversation.ParentConversationID(record)
	if !ok || wantParentID == "" {
		t.Fatalf("expected resolvable fork parent id, got (%q, %v)", wantParentID, ok)
	}
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: record, Stamp: stamp}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first"},
				{Role: "assistant", Timestamp: time.Unix(1710000030, 0), Text: "second"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1", len(client.upsertCalls))
	}
	docs := client.upsertCalls[0].Docs
	if len(docs) != 2 {
		t.Fatalf("upsert docs = %d, want 2", len(docs))
	}
	for index, doc := range docs {
		if doc.ParentConversationID != wantParentID {
			t.Fatalf("doc[%d] ParentConversationID = %q, want %q", index, doc.ParentConversationID, wantParentID)
		}
	}
}

func TestConversationSemanticSyncLeavesParentEmptyWithoutLineage(t *testing.T) {
	conversationID := "codex:no-lineage"
	stamp := semanticTestStamp(20, 200)
	record := semanticTestRecord(conversationID)
	if _, ok := conversation.ParentConversationID(record); ok {
		t.Fatalf("expected no resolvable parent for record without lineage")
	}
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: record, Stamp: stamp}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1", len(client.upsertCalls))
	}
	docs := client.upsertCalls[0].Docs
	if len(docs) != 1 {
		t.Fatalf("upsert docs = %d, want 1", len(docs))
	}
	if docs[0].ParentConversationID != "" {
		t.Fatalf("doc ParentConversationID = %q, want empty", docs[0].ParentConversationID)
	}
}

func semanticTestRecord(conversationID string) conversation.Record {
	return conversation.Record{
		ID:            conversationID,
		Provider:      conversation.ProviderCodex,
		NativeID:      conversationID,
		Lineage:       nil,
		Title:         "semantic test",
		WorkspaceRoot: "",
		ArtifactPath:  "/tmp/clyde-semantic-test.jsonl",
		ArtifactKind:  "jsonl",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}
}

func semanticTestStamp(size int64, unixSeconds int64) conversation.FileStamp {
	return conversation.FileStamp{
		Size:  size,
		Mtime: time.Unix(unixSeconds, 0),
	}
}

func semanticTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// semanticTestWorkerWith builds a worker over two-conversation fixtures shared
// by the batching tests: each conversation has one single-message transcript
// whose text length the test controls.
func semanticTestIndexWithTexts(textsByID map[string]string) *fakeConversationSemanticIndex {
	records := make([]conversation.StampedRecord, 0, len(textsByID))
	messagesByID := make(map[string][]transcript.Message, len(textsByID))
	for conversationID, text := range textsByID {
		records = append(records, conversation.StampedRecord{
			Record: semanticTestRecord(conversationID),
			Stamp:  semanticTestStamp(int64(len(text)), 200),
		})
		messagesByID[conversationID] = []transcript.Message{
			{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: text},
		}
	}
	return &fakeConversationSemanticIndex{
		records:      records,
		messagesByID: messagesByID,
		loadOptions:  nil,
	}
}

// TestConversationSemanticSyncStreamsAllNeededInOnePass proves the worker no
// longer splits the needed set by a byte budget: streaming carries every needed
// conversation's documents in one upsert, which the stream client frames into
// bounded chunks on the wire.
func TestConversationSemanticSyncStreamsAllNeededInOnePass(t *testing.T) {
	firstID := "codex:aaa"
	secondID := "codex:bbb"
	index := semanticTestIndexWithTexts(map[string]string{
		firstID:  "0123456789",
		secondID: "0123456789",
	})
	client := &fakeConversationSemanticClient{needed: []string{firstID, secondID}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1 (single streamed upsert)", len(client.upsertCalls))
	}
	docs := client.upsertCalls[0].Docs
	deliveredIDs := map[string]bool{}
	for _, doc := range docs {
		deliveredIDs[doc.ConversationID] = true
	}
	if !deliveredIDs[firstID] || !deliveredIDs[secondID] {
		t.Fatalf("delivered docs = %+v, want both %s and %s in one stream", docs, firstID, secondID)
	}
}

// TestConversationSemanticSyncShipsLargeConversationWhole proves a single large
// conversation ships in one upsert, never split: the engine replaces a
// conversation atomically, so streaming must deliver all of its documents
// together.
func TestConversationSemanticSyncShipsLargeConversationWhole(t *testing.T) {
	conversationID := "codex:oversized"
	index := semanticTestIndexWithTexts(map[string]string{
		conversationID: "0123456789012345678901234567890123456789",
	})
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1", len(client.upsertCalls))
	}
	docs := client.upsertCalls[0].Docs
	if len(docs) != 1 || docs[0].ConversationID != conversationID {
		t.Fatalf("delivered docs = %+v, want the large conversation whole", docs)
	}
}

// TestConversationSemanticSyncSkipsPassWhileEngineJobRuns proves the worker
// skips whole passes while the engine job from its last accepted upsert is
// still running: no manifest sync and no transcript reads, instead of loading
// documents only to be rejected. Once the job completes, passes resume.
func TestConversationSemanticSyncSkipsPassWhileEngineJobRuns(t *testing.T) {
	conversationID := "codex:busy"
	index := semanticTestIndexWithTexts(map[string]string{conversationID: "text"})
	client := &fakeConversationSemanticClient{
		needed:    []string{conversationID},
		jobStates: map[string]string{"upsert-job": "running"},
	}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("first runPass returned error: %v", err)
	}
	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls after first pass = %d, want 1", len(client.upsertCalls))
	}
	loadsAfterFirstPass := len(index.loadOptions)

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("second runPass returned error: %v", err)
	}
	if len(client.syncCalls) != 1 {
		t.Fatalf("sync calls after busy pass = %d, want 1 (pass skipped)", len(client.syncCalls))
	}
	if len(index.loadOptions) != loadsAfterFirstPass {
		t.Fatalf("transcript loads after busy pass = %d, want unchanged %d", len(index.loadOptions), loadsAfterFirstPass)
	}

	client.jobStates["upsert-job"] = "completed"
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("third runPass returned error: %v", err)
	}
	if len(client.syncCalls) != 2 {
		t.Fatalf("sync calls after job completion = %d, want 2 (pass resumed)", len(client.syncCalls))
	}
}

// TestConversationSemanticSyncDefersActivelyGrowingConversation proves a
// conversation whose transcript changed within the last interval is not
// delivered: the active session re-fingerprints on every pass, and delivering
// it would re-send a growing transcript each minute. It ships after one quiet
// interval.
func TestConversationSemanticSyncDefersActivelyGrowingConversation(t *testing.T) {
	conversationID := "codex:active"
	fixedNow := time.Unix(1710000600, 0)
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{
			Record: semanticTestRecord(conversationID),
			Stamp:  conversation.FileStamp{Size: 10, Mtime: fixedNow.Add(-2 * time.Second)},
		}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "growing"}},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())
	worker.now = func() time.Time { return fixedNow }

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}
	if len(client.upsertCalls) != 0 {
		t.Fatalf("upsert calls = %d, want 0 (active conversation deferred)", len(client.upsertCalls))
	}
	if len(index.loadOptions) != 0 {
		t.Fatalf("transcript loads = %d, want 0 (deferred before load)", len(index.loadOptions))
	}

	// One quiet interval later the same stamp is old enough to deliver.
	worker.now = func() time.Time { return fixedNow.Add(worker.interval) }
	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("second runPass returned error: %v", err)
	}
	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls after quiet interval = %d, want 1", len(client.upsertCalls))
	}
}

// TestConversationSemanticSyncDeliversEveryNeededConversation proves a pass now
// streams every needed conversation in one upsert and advances the delivery
// cursor to the last delivered id, so no needed conversation is starved.
func TestConversationSemanticSyncDeliversEveryNeededConversation(t *testing.T) {
	index := semanticTestIndexWithTexts(map[string]string{
		"codex:a": "text-a",
		"codex:b": "text-b",
		"codex:c": "text-c",
	})
	client := &fakeConversationSemanticClient{needed: []string{"codex:a", "codex:b", "codex:c"}}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1 (all needed in one stream)", len(client.upsertCalls))
	}
	deliveredIDs := map[string]bool{}
	for _, doc := range client.upsertCalls[0].Docs {
		deliveredIDs[doc.ConversationID] = true
	}
	for _, wantID := range []string{"codex:a", "codex:b", "codex:c"} {
		if !deliveredIDs[wantID] {
			t.Fatalf("delivered ids = %v, missing %s", deliveredIDs, wantID)
		}
	}
	if worker.deliveryCursor != "codex:c" {
		t.Fatalf("delivery cursor = %q, want codex:c (last delivered)", worker.deliveryCursor)
	}
}
