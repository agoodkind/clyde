package semsearch

import (
	"context"
	"testing"

	lmsemanticsearchv1 "goodkind.io/lm-semantic-search/gen/go/lmsemanticsearch/v1"
	"google.golang.org/grpc"
)

// fakeUpsertStreamClient captures the chunks sendUpsertStream sends. It embeds
// the generated client-streaming interface so the unused stream methods satisfy
// the type; only Send and CloseAndRecv are exercised.
type fakeUpsertStreamClient struct {
	grpc.ClientStreamingClient[lmsemanticsearchv1.UpsertConversationDocumentsChunk, lmsemanticsearchv1.UpsertConversationDocumentsResponse]
	sent []*lmsemanticsearchv1.UpsertConversationDocumentsChunk
}

func (f *fakeUpsertStreamClient) Send(chunk *lmsemanticsearchv1.UpsertConversationDocumentsChunk) error {
	f.sent = append(f.sent, chunk)
	return nil
}

func (f *fakeUpsertStreamClient) CloseAndRecv() (*lmsemanticsearchv1.UpsertConversationDocumentsResponse, error) {
	return &lmsemanticsearchv1.UpsertConversationDocumentsResponse{JobId: "job-test", DisplayText: ""}, nil
}

// TestSendUpsertStreamDeclaresRetainReconcileMode proves clyde declares RETAIN on
// the upsert header, so the engine keeps a conversation absent from the manifest
// rather than deleting it. This is the additive-only contract stated explicitly
// on the wire rather than relying on the engine's default.
func TestSendUpsertStreamDeclaresRetainReconcileMode(t *testing.T) {
	t.Parallel()

	stream := &fakeUpsertStreamClient{ClientStreamingClient: nil, sent: nil}
	manifest := []Fingerprint{{ConversationID: "conv-1", Value: "fp-1"}}
	if err := sendUpsertStream(context.Background(), stream, "collection-test", nil, manifest); err != nil {
		t.Fatalf("sendUpsertStream returned error: %v", err)
	}

	var header *lmsemanticsearchv1.UpsertConversationDocumentsHeader
	for _, chunk := range stream.sent {
		if h := chunk.GetHeader(); h != nil {
			header = h
		}
	}
	if header == nil {
		t.Fatal("sendUpsertStream sent no header chunk")
	}
	if got := header.GetReconcileMode(); got != lmsemanticsearchv1.ConversationReconcileMode_CONVERSATION_RECONCILE_MODE_RETAIN {
		t.Fatalf("upsert header reconcile mode = %v, want CONVERSATION_RECONCILE_MODE_RETAIN", got)
	}
}
