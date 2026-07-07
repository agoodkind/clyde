package semsearch

import (
	"context"
	"strings"
	"testing"

	lmsemanticsearchv1 "goodkind.io/lm-semantic-search/gen/go/lmsemanticsearch/v1"
	"google.golang.org/grpc"
)

// fakeUpsertStreamClient captures the chunks sendUpsertStream sends. It embeds
// the generated client-streaming interface so the stream methods satisfy the
// type; only Send is exercised, since sendUpsertStream never calls CloseAndRecv.
type fakeUpsertStreamClient struct {
	grpc.ClientStreamingClient[lmsemanticsearchv1.UpsertConversationDocumentsChunk, lmsemanticsearchv1.UpsertConversationDocumentsResponse]
	sent []*lmsemanticsearchv1.UpsertConversationDocumentsChunk
}

func (f *fakeUpsertStreamClient) Send(chunk *lmsemanticsearchv1.UpsertConversationDocumentsChunk) error {
	f.sent = append(f.sent, chunk)
	return nil
}

// TestSendUpsertStreamDeclaresRetainReconcileMode asserts clyde sets RETAIN on the
// upsert header rather than relying on the engine's default, so the additive-only
// intent is stated explicitly on the wire. The engine's own tests cover what
// RETAIN does; this only checks what clyde sends.
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

func TestSendUpsertStreamSplitsDocumentsUnderSafeByteBudget(t *testing.T) {
	t.Parallel()

	const safeByteBudget = 3 << 20
	largeText := strings.Repeat("x", safeByteBudget/2)
	stream := &fakeUpsertStreamClient{ClientStreamingClient: nil, sent: nil}
	docs := []SemDoc{
		{ConversationID: "codex:one", MessageIndex: 0, Role: "user", Text: largeText},
		{ConversationID: "codex:two", MessageIndex: 0, Role: "user", Text: largeText},
		{ConversationID: "codex:three", MessageIndex: 0, Role: "user", Text: largeText},
	}

	if err := sendUpsertStream(context.Background(), stream, "collection-test", docs, nil); err != nil {
		t.Fatalf("sendUpsertStream returned error: %v", err)
	}

	documentChunks := make([]*lmsemanticsearchv1.UpsertConversationDocumentsDocuments, 0)
	for _, chunk := range stream.sent {
		if documents := chunk.GetDocuments(); documents != nil {
			documentChunks = append(documentChunks, documents)
		}
	}
	if len(documentChunks) < 2 {
		t.Fatalf("document chunks = %d, want at least 2 chunks under %d bytes", len(documentChunks), safeByteBudget)
	}
	documentCount := 0
	for _, chunk := range documentChunks {
		chunkBytes := protoDocumentChunkByteSize(chunk.GetDocuments())
		if chunkBytes > safeByteBudget {
			t.Fatalf("document chunk bytes = %d, want <= %d", chunkBytes, safeByteBudget)
		}
		documentCount += len(chunk.GetDocuments())
	}
	if documentCount != len(docs) {
		t.Fatalf("streamed documents = %d, want %d", documentCount, len(docs))
	}
}

func TestConversationDocumentsCarriesToolsAndThinking(t *testing.T) {
	t.Parallel()

	documents := conversationDocuments([]SemDoc{
		{
			ConversationID: "codex:tools",
			MessageIndex:   7,
			Text:           "assistant prose",
			Thinking:       "reasoning text",
			Tools: []SemToolCall{
				{
					Name:      "Bash\xff",
					InputJSON: `{"command":"printf hi"}` + "\xff",
					Command:   "printf hi\xff",
					LangHint:  "bash\xff",
					Output:    "hi\xff",
					IsError:   true,
				},
			},
		},
	})

	if len(documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(documents))
	}
	document := documents[0]
	if document.GetThinking() != "reasoning text" {
		t.Fatalf("thinking = %q, want reasoning text", document.GetThinking())
	}
	if len(document.GetTools()) != 1 {
		t.Fatalf("tools = %d, want 1", len(document.GetTools()))
	}
	tool := document.GetTools()[0]
	if tool.GetName() != "Bash" {
		t.Fatalf("tool name = %q, want Bash", tool.GetName())
	}
	if tool.GetInputJson() != `{"command":"printf hi"}` {
		t.Fatalf("tool input_json = %q, want command JSON", tool.GetInputJson())
	}
	if tool.GetCommand() != "printf hi" {
		t.Fatalf("tool command = %q, want printf hi", tool.GetCommand())
	}
	if tool.GetLangHint() != "bash" {
		t.Fatalf("tool lang_hint = %q, want bash", tool.GetLangHint())
	}
	if tool.GetOutput() != "hi" {
		t.Fatalf("tool output = %q, want hi", tool.GetOutput())
	}
	if !tool.GetIsError() {
		t.Fatal("tool is_error = false, want true")
	}
}

func protoDocumentChunkByteSize(documents []*lmsemanticsearchv1.ConversationDocument) int {
	size := 0
	for _, document := range documents {
		size += semDocByteSize(SemDoc{
			ConversationID:       document.GetConversationId(),
			ParentConversationID: document.GetParentConversationId(),
			MessageIndex:         document.GetMessageIndex(),
			Role:                 document.GetRole(),
			TimestampUnix:        document.GetTimestampUnix(),
			Text:                 document.GetText(),
			Tools:                protoToolCalls(document.GetTools()),
			Thinking:             document.GetThinking(),
			WorkspaceRoot:        document.GetWorkspaceRoot(),
			Archived:             document.GetArchived(),
		})
	}
	return size
}

func protoToolCalls(tools []*lmsemanticsearchv1.ConversationToolCall) []SemToolCall {
	out := make([]SemToolCall, 0, len(tools))
	for _, tool := range tools {
		out = append(out, SemToolCall{
			Name:      tool.GetName(),
			InputJSON: tool.GetInputJson(),
			Command:   tool.GetCommand(),
			LangHint:  tool.GetLangHint(),
			Output:    tool.GetOutput(),
			IsError:   tool.GetIsError(),
		})
	}
	return out
}
