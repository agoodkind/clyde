package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
)

type semanticUpsertCall struct {
	CollectionID string
	Docs         []semsearch.SemDoc
}

type semanticDeleteCall struct {
	CollectionID   string
	ConversationID string
}

type fakeConversationSemanticClient struct {
	upsertCalls   []semanticUpsertCall
	deleteCalls   []semanticDeleteCall
	jobStateByID  map[string]string
	waitedJobIDs  []string
	terminalState string
}

func (c *fakeConversationSemanticClient) resolvedTerminalState() string {
	if c.terminalState == "" {
		return semsearch.JobStateCompleted
	}
	return c.terminalState
}

func (c *fakeConversationSemanticClient) UpsertConversationDocuments(_ context.Context, collectionID string, docs []semsearch.SemDoc) (string, error) {
	copiedDocs := append([]semsearch.SemDoc(nil), docs...)
	c.upsertCalls = append(c.upsertCalls, semanticUpsertCall{
		CollectionID: collectionID,
		Docs:         copiedDocs,
	})
	return "upsert-job", nil
}

func (c *fakeConversationSemanticClient) DeleteConversation(_ context.Context, collectionID, conversationID string) (string, error) {
	c.deleteCalls = append(c.deleteCalls, semanticDeleteCall{
		CollectionID:   collectionID,
		ConversationID: conversationID,
	})
	return "delete-job", nil
}

func (c *fakeConversationSemanticClient) JobState(_ context.Context, jobID string) (string, error) {
	if c.jobStateByID != nil {
		if state, ok := c.jobStateByID[jobID]; ok {
			return state, nil
		}
	}
	return c.resolvedTerminalState(), nil
}

func (c *fakeConversationSemanticClient) WaitForJob(_ context.Context, jobID string, _, _ time.Duration) (string, error) {
	c.waitedJobIDs = append(c.waitedJobIDs, jobID)
	if c.jobStateByID != nil {
		if state, ok := c.jobStateByID[jobID]; ok {
			return state, nil
		}
	}
	return c.resolvedTerminalState(), nil
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

func TestConversationSemanticSyncUpsertsChangedConversation(t *testing.T) {
	conversationID := "codex:one"
	oldStamp := semanticTestStamp(10, 100)
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
	client := &fakeConversationSemanticClient{}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())
	worker.lastPushed[conversationID] = oldStamp

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1", len(client.upsertCalls))
	}
	call := client.upsertCalls[0]
	if call.CollectionID != "collection-test" {
		t.Fatalf("collection id = %q, want collection-test", call.CollectionID)
	}
	if len(call.Docs) != 2 {
		t.Fatalf("upsert docs = %d, want 2", len(call.Docs))
	}
	if call.Docs[0].ConversationID != conversationID || call.Docs[1].ConversationID != conversationID {
		t.Fatalf("conversation ids = %q, %q; want %q", call.Docs[0].ConversationID, call.Docs[1].ConversationID, conversationID)
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
	if call.Docs[0].TimestampUnix != 1710000000 || call.Docs[1].TimestampUnix != 1710000030 {
		t.Fatalf("timestamps = %d, %d; want 1710000000, 1710000030", call.Docs[0].TimestampUnix, call.Docs[1].TimestampUnix)
	}
	if len(index.loadOptions) != 1 {
		t.Fatalf("load options calls = %d, want 1", len(index.loadOptions))
	}
	opts := index.loadOptions[0]
	if opts.IncludeSystemPrompts || opts.IncludeSystemMessages || opts.IncludeToolOutputs {
		t.Fatalf("load options = %+v, want all semantic projection flags false", opts)
	}
	if pushedStamp, ok := worker.lastPushed[conversationID]; !ok || !pushedStamp.Equal(newStamp) {
		t.Fatalf("last pushed stamp = %+v, %t; want %+v", pushedStamp, ok, newStamp)
	}
}

func TestConversationSemanticSyncSkipsUnchangedConversation(t *testing.T) {
	conversationID := "codex:stable"
	stamp := semanticTestStamp(30, 300)
	record := semanticTestRecord(conversationID)
	index := &fakeConversationSemanticIndex{
		records:      []conversation.StampedRecord{{Record: record, Stamp: stamp}},
		messagesByID: map[string][]transcript.Message{},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())
	worker.lastPushed[conversationID] = stamp

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 0 {
		t.Fatalf("upsert calls = %d, want 0", len(client.upsertCalls))
	}
	if len(index.loadOptions) != 0 {
		t.Fatalf("load calls = %d, want 0", len(index.loadOptions))
	}
}

func TestConversationSemanticSyncDeletesRemovedConversation(t *testing.T) {
	conversationID := "codex:removed"
	index := &fakeConversationSemanticIndex{
		records:      nil,
		messagesByID: map[string][]transcript.Message{},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())
	worker.lastPushed[conversationID] = semanticTestStamp(40, 400)

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.deleteCalls) != 1 {
		t.Fatalf("delete calls = %d, want 1", len(client.deleteCalls))
	}
	call := client.deleteCalls[0]
	if call.CollectionID != "collection-test" {
		t.Fatalf("collection id = %q, want collection-test", call.CollectionID)
	}
	if call.ConversationID != conversationID {
		t.Fatalf("conversation id = %q, want %q", call.ConversationID, conversationID)
	}
	if _, ok := worker.lastPushed[conversationID]; ok {
		t.Fatalf("last pushed still contains %q after delete", conversationID)
	}
}

func TestConversationSemanticSyncBatchesMultipleChangedConversations(t *testing.T) {
	firstID := "codex:one"
	secondID := "claude:two"
	firstStamp := semanticTestStamp(20, 200)
	secondStamp := semanticTestStamp(21, 210)
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{
			{Record: semanticTestRecord(firstID), Stamp: firstStamp},
			{Record: semanticTestRecord(secondID), Stamp: secondStamp},
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
	client := &fakeConversationSemanticClient{}
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
	docConversationIDs := map[string]int{}
	for _, doc := range call.Docs {
		docConversationIDs[doc.ConversationID]++
	}
	if docConversationIDs[firstID] != 1 || docConversationIDs[secondID] != 2 {
		t.Fatalf("docs per conversation = %v, want %s:1 %s:2", docConversationIDs, firstID, secondID)
	}
	if len(client.waitedJobIDs) != 1 || client.waitedJobIDs[0] != "upsert-job" {
		t.Fatalf("waited job ids = %v, want exactly [upsert-job]", client.waitedJobIDs)
	}
	if pushed, ok := worker.lastPushed[firstID]; !ok || !pushed.Equal(firstStamp) {
		t.Fatalf("first conversation stamp = %+v, %t; want %+v", pushed, ok, firstStamp)
	}
	if pushed, ok := worker.lastPushed[secondID]; !ok || !pushed.Equal(secondStamp) {
		t.Fatalf("second conversation stamp = %+v, %t; want %+v", pushed, ok, secondStamp)
	}
}

func TestConversationSemanticSyncDoesNotMarkStampsWhenUpsertJobFails(t *testing.T) {
	conversationID := "codex:one"
	oldStamp := semanticTestStamp(10, 100)
	newStamp := semanticTestStamp(20, 200)
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: semanticTestRecord(conversationID), Stamp: newStamp}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{terminalState: semsearch.JobStateFailed}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())
	worker.lastPushed[conversationID] = oldStamp

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1", len(client.upsertCalls))
	}
	pushed, ok := worker.lastPushed[conversationID]
	if !ok || !pushed.Equal(oldStamp) {
		t.Fatalf("last pushed stamp = %+v, %t; want unchanged %+v so the next pass retries", pushed, ok, oldStamp)
	}
}

func TestConversationSemanticSyncSkipsPassWhileJobInFlight(t *testing.T) {
	conversationID := "codex:one"
	oldStamp := semanticTestStamp(10, 100)
	newStamp := semanticTestStamp(20, 200)
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: semanticTestRecord(conversationID), Stamp: newStamp}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "first"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{
		jobStateByID: map[string]string{"stuck-job": "running"},
	}
	worker := newConversationSemanticSyncWorker(index, client, "collection-test", semanticTestLogger())
	worker.lastPushed[conversationID] = oldStamp
	worker.inFlightJobID = "stuck-job"

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error while job in flight: %v", err)
	}
	if len(client.upsertCalls) != 0 || len(client.deleteCalls) != 0 {
		t.Fatalf("upsert=%d delete=%d, want both 0 while job in flight", len(client.upsertCalls), len(client.deleteCalls))
	}
	if worker.inFlightJobID != "stuck-job" {
		t.Fatalf("inFlightJobID = %q, want stuck-job preserved while running", worker.inFlightJobID)
	}
	if pushed, ok := worker.lastPushed[conversationID]; !ok || !pushed.Equal(oldStamp) {
		t.Fatalf("last pushed stamp = %+v, %t; want unchanged %+v while skipped", pushed, ok, oldStamp)
	}

	client.jobStateByID["stuck-job"] = semsearch.JobStateCompleted

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error after job completed: %v", err)
	}
	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1 after in-flight job completed", len(client.upsertCalls))
	}
	if worker.inFlightJobID != "" {
		t.Fatalf("inFlightJobID = %q, want cleared after terminal upsert", worker.inFlightJobID)
	}
	if pushed, ok := worker.lastPushed[conversationID]; !ok || !pushed.Equal(newStamp) {
		t.Fatalf("last pushed stamp = %+v, %t; want %+v after pass proceeded", pushed, ok, newStamp)
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
	client := &fakeConversationSemanticClient{}
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
	client := &fakeConversationSemanticClient{}
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
