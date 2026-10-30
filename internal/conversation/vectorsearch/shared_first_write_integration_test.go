//go:build milvusintegration

package vectorsearch_test

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	firstWriteCollectionID   = "integration-shared-first-write"
	firstWriteConversationID = "codex:first-write"
	firstWriteText           = "The restic forget policy keeps fourteen daily snapshots."
	firstWriteTimestamp      = int64(1_790_002_000)
)

// TestSharedLayoutFirstWriteCreatesAndLoadsCollections checks that a shared layout
// client can write a conversation message to a collection that does not yet exist
// and return the message with its timestamp in a search. The test uses a temporary
// Milvus stack and the real embedding endpoint.
func TestSharedLayoutFirstWriteCreatesAndLoadsCollections(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	shared := openClient(t, address, vectorsearch.LayoutShared)
	ctx := context.Background()
	docs := []semsearch.SemDoc{{
		ConversationID: firstWriteConversationID, ParentConversationID: "", MessageIndex: 0, Role: "user",
		TimestampUnix: firstWriteTimestamp, Text: firstWriteText, Tools: nil, Thinking: "",
		WorkspaceRoot: "/work/0", Archived: false, LoadRules: "",
	}}
	manifest := []semsearch.Fingerprint{{ConversationID: firstWriteConversationID, Value: "v1"}}
	if _, err := shared.UpsertConversationDocuments(ctx, firstWriteCollectionID, docs, manifest); err != nil {
		t.Fatalf("write to a new shared collection: %v", err)
	}
	time.Sleep(visibilityWait)
	hits, err := shared.SearchConversations(ctx, firstWriteCollectionID, "restic forget policy daily snapshots", 3, semsearch.SearchFilter{}, 0)
	if err != nil {
		t.Fatalf("search the new shared collection: %v", err)
	}
	if len(hits) == 0 || hits[0].ConversationID != firstWriteConversationID || hits[0].TimestampUnix != firstWriteTimestamp {
		t.Fatalf("search did not return the written message first with its timestamp; hits: %+v", hits)
	}
}
