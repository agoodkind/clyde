package daemon

import (
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func TestExportTranscriptRequestIncludesToolSummaries(t *testing.T) {
	t.Parallel()
	req := exportTranscriptRequest("claude:probe", conversation.ExportOptions{
		Format:       conversation.ExportFormatMarkdown,
		HistoryStart: 0,
		Whitespace:   conversation.WhitespacePreserve,
		Content:      conversation.NewContentKindSet(conversation.ContentKindToolSummaries),
	})
	if !req.GetIncludeToolSummaries() {
		t.Fatal("IncludeToolSummaries = false, want true")
	}
	if req.GetIncludeToolCalls() {
		t.Fatal("IncludeToolCalls = true, want false")
	}
	if req.GetIncludeToolOutputs() {
		t.Fatal("IncludeToolOutputs = true, want false")
	}
}

func TestContentKindSetFromExportRequestIncludesToolSummaries(t *testing.T) {
	t.Parallel()
	set := contentKindSetFromExportRequest(&clydev1.ExportTranscriptRequest{
		IncludeChat:          true,
		IncludeToolSummaries: true,
	})
	if !set.Has(conversation.ContentKindChat) {
		t.Fatal("content set missing chat")
	}
	if !set.Has(conversation.ContentKindToolSummaries) {
		t.Fatal("content set missing tool summaries")
	}
}

func TestContentKindSetFromExportRequestUsesToolPrecedence(t *testing.T) {
	t.Parallel()
	set := contentKindSetFromExportRequest(&clydev1.ExportTranscriptRequest{
		IncludeToolSummaries: true,
		IncludeToolCalls:     true,
		IncludeToolOutputs:   true,
	})
	if !set.Has(conversation.ContentKindToolOutputs) {
		t.Fatal("content set missing tool outputs")
	}
	if set.Has(conversation.ContentKindToolCalls) {
		t.Fatal("content set kept tool calls when tool outputs was selected")
	}
	if set.Has(conversation.ContentKindToolSummaries) {
		t.Fatal("content set kept tool summaries when tool outputs was selected")
	}
}
