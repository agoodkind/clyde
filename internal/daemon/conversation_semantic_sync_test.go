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
	upsertCalls []semanticUpsertCall
	deleteCalls []semanticDeleteCall
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
