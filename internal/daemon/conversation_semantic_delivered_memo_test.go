package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
)

// TestSuccessfulDeliveryRecordsProjectionMemo proves an accepted upsert stores
// each delivered conversation's fingerprint and projection hash, which is the
// state a later pass needs to recognize a stamp change without a content
// change.
func TestSuccessfulDeliveryRecordsProjectionMemo(t *testing.T) {
	conversationID := "codex:memo-a"
	stamp := semanticTestStamp(20, 200)
	record := semanticTestRecord(conversationID)
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: record, Stamp: stamp}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "alpha"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	memo, recorded := worker.deliveredContent[conversationID]
	if !recorded {
		t.Fatalf("deliveredContent has no entry for %q after an accepted upsert", conversationID)
	}
	if memo.fingerprint != conversation.ContentFingerprint(record, stamp) {
		t.Fatalf("memo fingerprint = %q, want the advertised fingerprint %q", memo.fingerprint, conversation.ContentFingerprint(record, stamp))
	}
	if memo.projectionHash == "" {
		t.Fatal("memo projection hash is empty, want the delivered projection hash")
	}
}

// TestRejectedDeliveryRecordsNoMemo proves a failed upsert stores nothing: the
// engine checkpoint did not advance, so remembering the delivery would let a
// later pass pin a fingerprint the engine never accepted.
func TestRejectedDeliveryRecordsNoMemo(t *testing.T) {
	conversationID := "codex:memo-reject"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: semanticTestRecord(conversationID), Stamp: semanticTestStamp(20, 200)}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "alpha"},
			},
		},
		loadOptions: nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}, upsertErr: errors.New("engine rejected the upsert")}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}
	if len(worker.deliveredContent) != 0 {
		t.Fatalf("deliveredContent = %v after a rejected upsert, want empty", worker.deliveredContent)
	}
}

// TestProjectionHashSeparatesContentFromBoundaries pins the hash contract:
// identical projections hash equal, any content difference hashes differently,
// and field boundaries cannot collide.
func TestProjectionHashSeparatesContentFromBoundaries(t *testing.T) {
	t.Parallel()
	base := []semsearch.SemDoc{{
		ConversationID: "c",
		MessageIndex:   0,
		Role:           "user",
		Text:           "ab",
		Thinking:       "cd",
	}}
	same := []semsearch.SemDoc{{
		ConversationID: "c",
		MessageIndex:   0,
		Role:           "user",
		Text:           "ab",
		Thinking:       "cd",
	}}
	shifted := []semsearch.SemDoc{{
		ConversationID: "c",
		MessageIndex:   0,
		Role:           "user",
		Text:           "abc",
		Thinking:       "d",
	}}
	if SemanticProjectionHash(base) != SemanticProjectionHash(same) {
		t.Fatal("identical projections hash differently")
	}
	if SemanticProjectionHash(base) == SemanticProjectionHash(shifted) {
		t.Fatal("boundary-shifted projection hashes equal, want a distinct hash")
	}
}

func TestConversationSemanticSyncDeliversChangedMetadata(t *testing.T) {
	for _, field := range []string{"archived", "workspace", "timestamp", "tool_language", "tool_error"} {
		t.Run(field, func(t *testing.T) {
			conversationID := "codex:metadata"
			index := semanticTestIndexWithTexts(map[string]string{conversationID: "unchanged text"})
			index.messagesByID[conversationID][0].Tools = []transcript.ToolCall{{Name: "shell", Display: "date", DisplayLang: "bash"}}
			client := &fakeConversationSemanticClient{needed: []string{conversationID}}
			worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())
			if err := worker.runPass(context.Background()); err != nil {
				t.Fatal(err)
			}
			index.records[0].Stamp = semanticTestStamp(100, 500)
			switch field {
			case "archived":
				index.records[0].Record.Archived = true
			case "workspace":
				index.records[0].Record.WorkspaceRoot = "/moved-workspace"
			case "timestamp":
				index.messagesByID[conversationID][0].Timestamp = time.Unix(1710000100, 0)
			case "tool_language":
				index.messagesByID[conversationID][0].Tools[0].DisplayLang = "zsh"
			case "tool_error":
				index.messagesByID[conversationID][0].Tools[0].IsError = true
			}
			if err := worker.runPass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(client.upsertCalls) != 2 {
				t.Fatalf("upserts = %d, want metadata update after original delivery", len(client.upsertCalls))
			}
			actual := client.upsertCalls[1].Docs
			expected, err := BuildSemanticConversationDocuments(index.records[0].Record, index.messagesByID[conversationID], semanticTestContentKinds())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected.Docs) {
				t.Fatalf("delivered metadata = %+v, want %+v", actual, expected.Docs)
			}
		})
	}
}
