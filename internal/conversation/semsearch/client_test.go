package semsearch

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

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
	if err := sendUpsertStream(context.Background(), stream, "collection-test", nil, manifest, false); err != nil {
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
	if header.GetReexamineDelivered() {
		t.Fatal("upsert header reexamine_delivered = true, want false for a normal upsert")
	}
}

func TestSendUpsertStreamSetsReexamineDeliveredWhenRequested(t *testing.T) {
	t.Parallel()

	stream := &fakeUpsertStreamClient{ClientStreamingClient: nil, sent: nil}
	manifest := []Fingerprint{{ConversationID: "conv-1", Value: "fp-1"}}
	if err := sendUpsertStream(context.Background(), stream, "collection-test", nil, manifest, true); err != nil {
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
	if !header.GetReexamineDelivered() {
		t.Fatal("upsert header reexamine_delivered = false, want true when reexamine requested")
	}
	// Re-examination must not change the retain semantics: a delivered conversation
	// set still keeps every conversation the manifest omits.
	if got := header.GetReconcileMode(); got != lmsemanticsearchv1.ConversationReconcileMode_CONVERSATION_RECONCILE_MODE_RETAIN {
		t.Fatalf("upsert header reconcile mode = %v, want CONVERSATION_RECONCILE_MODE_RETAIN", got)
	}
}

func TestTruncateSemDocForUpsertPreservesTextWhenThinkingOversized(t *testing.T) {
	t.Parallel()

	// A document oversized almost entirely by its Thinking block must keep its
	// searchable Text intact: shrinking Thinking alone brings it under budget, so
	// Text should never be cut to make room for Thinking.
	text := "searchable text that must survive truncation"
	doc := SemDoc{
		ConversationID: "claude:thinking-heavy",
		MessageIndex:   0,
		Role:           "assistant",
		Text:           text,
		Thinking:       strings.Repeat("z", upsertStreamMaxBytesPerChunk*2),
	}

	out := truncateSemDocForUpsert(doc)

	if got := semDocByteSize(out); got > upsertStreamMaxBytesPerChunk {
		t.Fatalf("truncated doc size = %d, want <= %d", got, upsertStreamMaxBytesPerChunk)
	}
	if out.Text != text {
		t.Fatalf("Text was truncated to %q though shrinking Thinking alone fit the budget", out.Text)
	}
}

func TestTruncateSemDocStringToMaxBytesNeverExceedsBudget(t *testing.T) {
	t.Parallel()

	// When maxBytes is smaller than the truncation marker, the helper must still
	// return no more than maxBytes bytes and stay valid UTF-8, so the per-document
	// size guard can never be defeated by the marker itself.
	value := strings.Repeat("a", 100)
	for _, maxBytes := range []int{0, 1, 5, 10, 20} {
		got := truncateSemDocStringToMaxBytes(value, maxBytes)
		if len(got) > maxBytes {
			t.Fatalf("truncateSemDocStringToMaxBytes(_, %d) len = %d, want <= %d (%q)", maxBytes, len(got), maxBytes, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncateSemDocStringToMaxBytes(_, %d) = %q, not valid UTF-8", maxBytes, got)
		}
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

	if err := sendUpsertStream(context.Background(), stream, "collection-test", docs, nil, false); err != nil {
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

func TestSendUpsertStreamTruncatesSingleOversizedToolOutput(t *testing.T) {
	t.Parallel()

	stream := &fakeUpsertStreamClient{ClientStreamingClient: nil, sent: nil}
	docs := []SemDoc{
		{
			ConversationID: "codex:oversized",
			MessageIndex:   0,
			Role:           "assistant",
			Text:           "searchable assistant text",
			Tools: []SemToolCall{
				{
					Name:      "Bash",
					InputJSON: `{"command":"generate-large-output"}`,
					Command:   "generate-large-output",
					LangHint:  "bash",
					Output:    strings.Repeat("é", upsertStreamMaxBytesPerChunk/2+1024),
				},
			},
		},
	}

	if err := sendUpsertStream(context.Background(), stream, "collection-test", docs, nil, false); err != nil {
		t.Fatalf("sendUpsertStream returned error: %v", err)
	}

	documentChunks := make([]*lmsemanticsearchv1.UpsertConversationDocumentsDocuments, 0)
	for _, chunk := range stream.sent {
		if documents := chunk.GetDocuments(); documents != nil {
			documentChunks = append(documentChunks, documents)
		}
	}
	if len(documentChunks) != 1 {
		t.Fatalf("document chunks = %d, want 1", len(documentChunks))
	}
	documents := documentChunks[0].GetDocuments()
	if len(documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(documents))
	}
	chunkBytes := protoDocumentChunkByteSize(documents)
	if chunkBytes > upsertStreamMaxBytesPerChunk {
		t.Fatalf("document chunk bytes = %d, want <= %d", chunkBytes, upsertStreamMaxBytesPerChunk)
	}
	tool := documents[0].GetTools()[0]
	if tool.GetName() != "Bash" || tool.GetCommand() != "generate-large-output" || tool.GetLangHint() != "bash" {
		t.Fatalf("tool identity = %+v, want name, command, and lang hint intact", tool)
	}
	if !strings.Contains(tool.GetOutput(), "\n…[truncated ") {
		t.Fatalf("tool output was not truncated: suffix %q", tool.GetOutput()[len(tool.GetOutput())-64:])
	}
	if !utf8.ValidString(tool.GetOutput()) {
		t.Fatalf("tool output is not valid UTF-8")
	}
	if tool.GetInputJson() != `{"command":"generate-large-output"}` {
		t.Fatalf("tool input_json = %q, want intact input", tool.GetInputJson())
	}
}

func TestSendUpsertStreamDropsToolsWhenOverheadStaysOverBudget(t *testing.T) {
	t.Parallel()

	// Oversize dominated by non-shrinkable tool overhead: tool names never
	// truncate, so field truncation cannot reduce the document and the final
	// guard must drop the tool calls to keep the chunk under budget.
	oversizedName := strings.Repeat("n", upsertStreamMaxBytesPerChunk/2)
	stream := &fakeUpsertStreamClient{ClientStreamingClient: nil, sent: nil}
	docs := []SemDoc{
		{
			ConversationID: "codex:overhead",
			MessageIndex:   0,
			Role:           "assistant",
			Text:           "searchable assistant text",
			Tools: []SemToolCall{
				{Name: oversizedName, LangHint: "bash"},
				{Name: oversizedName, LangHint: "bash"},
				{Name: oversizedName, LangHint: "bash"},
			},
		},
	}

	if err := sendUpsertStream(context.Background(), stream, "collection-test", docs, nil, false); err != nil {
		t.Fatalf("sendUpsertStream returned error: %v", err)
	}

	documentChunks := make([]*lmsemanticsearchv1.UpsertConversationDocumentsDocuments, 0)
	for _, chunk := range stream.sent {
		if documents := chunk.GetDocuments(); documents != nil {
			documentChunks = append(documentChunks, documents)
		}
	}
	if len(documentChunks) != 1 {
		t.Fatalf("document chunks = %d, want 1", len(documentChunks))
	}
	documents := documentChunks[0].GetDocuments()
	if len(documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(documents))
	}
	chunkBytes := protoDocumentChunkByteSize(documents)
	if chunkBytes > upsertStreamMaxBytesPerChunk {
		t.Fatalf("document chunk bytes = %d, want <= %d", chunkBytes, upsertStreamMaxBytesPerChunk)
	}
	if got := len(documents[0].GetTools()); got != 0 {
		t.Fatalf("tools = %d, want 0 after overhead guard drops them", got)
	}
	if documents[0].GetText() != "searchable assistant text" {
		t.Fatalf("text = %q, want searchable assistant text intact", documents[0].GetText())
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
