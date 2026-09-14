package conversation

import (
	"encoding/json"
	"iter"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

const encryptedExportSentinel = "EXPORT_ENCRYPTED_SENTINEL"

func exportEncryptedReasoningContextItem() transcript.CompactedContextItem {
	raw := json.RawMessage(
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"readable reasoning summary"}],"encrypted_content":"` +
			encryptedExportSentinel +
			`"}`,
	)
	return transcript.CompactedContextItem{
		Kind:    transcript.CompactedContextItemKindReasoning,
		Message: nil,
		Reasoning: &transcript.CompactedReasoningItem{
			Summary: []transcript.CompactedReasoningSummary{
				{
					Type: "summary_text",
					Text: "readable reasoning summary",
					Raw: json.RawMessage(
						`{"type":"summary_text","text":"readable reasoning summary"}`,
					),
				},
			},
			SummaryRaw: json.RawMessage(
				`[{"type":"summary_text","text":"readable reasoning summary"}]`,
			),
			ContentRaw:       nil,
			EncryptedContent: encryptedExportSentinel,
			Raw:              raw,
		},
		LocalShellCall:       nil,
		FunctionCall:         nil,
		ToolSearchCall:       nil,
		FunctionCallOutput:   nil,
		CustomToolCall:       nil,
		CustomToolCallOutput: nil,
		ToolSearchOutput:     nil,
		WebSearchCall:        nil,
		ImageGenerationCall:  nil,
		Compaction:           nil,
		CompactionTrigger:    nil,
		ContextCompaction:    nil,
		Other:                nil,
	}
}

func exportTypedOnlyEncryptedReasoningItem() transcript.CompactedContextItem {
	item := exportEncryptedReasoningContextItem()
	reasoning := *item.Reasoning
	reasoning.Raw = json.RawMessage(
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"readable reasoning summary"}]}`,
	)
	item.Reasoning = &reasoning
	return item
}

func exportRawOnlyEncryptedReasoningItem() transcript.CompactedContextItem {
	item := exportEncryptedReasoningContextItem()
	reasoning := *item.Reasoning
	reasoning.EncryptedContent = ""
	item.Reasoning = &reasoning
	return item
}

func exportEncryptedOtherContextItem() transcript.CompactedContextItem {
	return transcript.CompactedContextItem{
		Kind: transcript.CompactedContextItemKindOther,
		Other: &transcript.CompactedOtherItem{
			Type: "mystery",
			Raw: json.RawMessage(
				`{"type":"mystery","encrypted_content":"` + encryptedExportSentinel + `"}`,
			),
		},
	}
}

func assertExportOmitsEncrypted(t *testing.T, body []byte) {
	t.Helper()
	text := string(body)
	if strings.Contains(text, encryptedExportSentinel) {
		t.Fatal("export included encrypted payload sentinel")
	}
	if strings.Contains(text, `"encrypted_content"`) {
		t.Fatal("export included encrypted_content field")
	}
}

func mustExportJSONRaw(t *testing.T, index *Index, record Record) []byte {
	t.Helper()
	body, err := index.Export(record, ExportOptions{
		Format:       ExportFormatJSON,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		MaxTokens:    "",
		TokenModel:   "",
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindSystemMessages, ContentKindRawJSONMetadata),
		Compaction: CompactionExportOptions{
			IncludeSelector: "0",
			FullHistory:     false,
		},
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return body
}

func newExportIndexWithMessages(messages []transcript.Message) (*Index, Record) {
	registry := NewRegistry()
	registry.Register(&streamFuncParser{stream: func(_ string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
		return func(yield func(transcript.Message, error) bool) {
			for _, message := range messages {
				if message.Role == "system" && !opts.IncludeSystemMessages {
					continue
				}
				if !yield(message, nil) {
					return
				}
			}
		}
	}})
	idx := newTestIndex(registry)
	idx.records = []Record{compactionExportRecord()}
	idx.loaded = true
	return idx, idx.records[0]
}

func withExtraBoundaryContext(item transcript.CompactedContextItem) []transcript.Message {
	messages := compactionExportMessages()
	for i := range messages {
		if messages[i].UUID != "boundary-3" || messages[i].Compaction == nil {
			continue
		}
		compaction := *messages[i].Compaction
		compaction.ContextItems = append(slices.Clone(compaction.ContextItems), item)
		compaction.ReplacementHistoryCount = len(compaction.ContextItems)
		messages[i].Compaction = &compaction
	}
	return messages
}

func TestExportOmitsEncryptedCompactionContent(t *testing.T) {
	t.Parallel()
	formats := []ExportFormat{
		ExportFormatMarkdown,
		ExportFormatHTML,
		ExportFormatPlainText,
		ExportFormatJSON,
	}
	for _, format := range formats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			index, record, _ := newCompactionExportIndex()
			body, err := index.Export(record, ExportOptions{
				Format:       format,
				HistoryStart: 0,
				LastN:        0,
				MaxLines:     0,
				MaxTokens:    "",
				TokenModel:   "",
				Whitespace:   WhitespacePreserve,
				Content:      NewContentKindSet(ContentKindChat),
				Compaction: CompactionExportOptions{
					IncludeSelector: "0",
					FullHistory:     false,
				},
			})
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			text := string(body)
			if strings.Contains(text, encryptedExportSentinel) {
				t.Fatal("export included encrypted payload sentinel")
			}
			if strings.Contains(text, `"encrypted_content"`) {
				t.Fatal("export included encrypted_content field")
			}
			if !strings.Contains(text, "readable reasoning summary") {
				t.Fatal("export removed readable reasoning summary")
			}
		})
	}
}

func TestExportRawJSONOmitsEncryptedCompactionContent(t *testing.T) {
	t.Parallel()
	index, record, _ := newCompactionExportIndex()
	body, err := index.Export(record, ExportOptions{
		Format:       ExportFormatJSON,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		MaxTokens:    "",
		TokenModel:   "",
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindSystemMessages, ContentKindRawJSONMetadata),
		Compaction: CompactionExportOptions{
			IncludeSelector: "0",
			FullHistory:     false,
		},
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	text := string(body)
	if strings.Contains(text, encryptedExportSentinel) {
		t.Fatal("export included encrypted payload sentinel")
	}
	if strings.Contains(text, `"encrypted_content"`) {
		t.Fatal("export included encrypted_content field")
	}
	if !strings.Contains(text, "readable reasoning summary") {
		t.Fatal("export removed readable reasoning summary")
	}
}

func TestExportOmitsTypedOnlyEncryptedContent(t *testing.T) {
	t.Parallel()
	index, record := newExportIndexWithMessages(
		withExtraBoundaryContext(exportTypedOnlyEncryptedReasoningItem()),
	)
	body := mustExportJSONRaw(t, index, record)
	assertExportOmitsEncrypted(t, body)
	if !strings.Contains(string(body), "readable reasoning summary") {
		t.Fatal("export removed readable reasoning summary")
	}
}

func TestExportOmitsRawOnlyEncryptedContent(t *testing.T) {
	t.Parallel()
	index, record := newExportIndexWithMessages(
		withExtraBoundaryContext(exportRawOnlyEncryptedReasoningItem()),
	)
	body := mustExportJSONRaw(t, index, record)
	assertExportOmitsEncrypted(t, body)
	if !strings.Contains(string(body), "readable reasoning summary") {
		t.Fatal("export removed readable reasoning summary")
	}
}

func TestExportOmitsNestedOtherRawEncryptedContent(t *testing.T) {
	t.Parallel()
	index, record := newExportIndexWithMessages(
		withExtraBoundaryContext(exportEncryptedOtherContextItem()),
	)
	body := mustExportJSONRaw(t, index, record)
	assertExportOmitsEncrypted(t, body)
	if !strings.Contains(string(body), "latest boundary context") {
		t.Fatal("export dropped surrounding compaction context")
	}
}

func TestExportOmitsEncryptedContentForFullHistory(t *testing.T) {
	t.Parallel()
	index, record, _ := newCompactionExportIndex()
	body, err := index.Export(record, ExportOptions{
		Format:       ExportFormatJSON,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		MaxTokens:    "",
		TokenModel:   "",
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindSystemMessages, ContentKindRawJSONMetadata),
		Compaction: CompactionExportOptions{
			IncludeSelector: "",
			FullHistory:     true,
		},
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	assertExportOmitsEncrypted(t, body)
	if !strings.Contains(string(body), "readable reasoning summary") {
		t.Fatal("export removed readable reasoning summary")
	}
}

func TestExportLeavesParserOwnedEncryptedContentUnchanged(t *testing.T) {
	t.Parallel()
	messages := compactionExportMessages()
	index, record := newExportIndexWithMessages(messages)
	_ = mustExportJSONRaw(t, index, record)
	found := false
	for _, message := range messages {
		if message.Compaction == nil {
			continue
		}
		for _, item := range message.Compaction.ContextItems {
			if item.Reasoning == nil {
				continue
			}
			found = true
			if item.Reasoning.EncryptedContent != encryptedExportSentinel {
				t.Fatal("export mutated parser-owned EncryptedContent")
			}
			if !strings.Contains(string(item.Reasoning.Raw), encryptedExportSentinel) {
				t.Fatal("export mutated parser-owned Raw")
			}
		}
	}
	if !found {
		t.Fatal("fixture missing parser-owned encrypted reasoning")
	}
}

func TestExportDefaultUsesLatestCompactionSegment(t *testing.T) {
	t.Parallel()
	idx, record, loadOptions := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	if len(*loadOptions) != 1 {
		t.Fatalf("load options calls = %d, want 1", len(*loadOptions))
	}
	if !(*loadOptions)[0].IncludeSystemMessages {
		t.Fatalf("default segment export did not load system messages")
	}
	text := string(body)
	for _, want := range []string{
		"Compaction Segments 0",
		"latest boundary context",
		"latest summary context",
		"tail after latest",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("default segment export missing %q in:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"tail after first", "tail after micro", "latest boundary text"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("default segment export included %q in:\n%s", unwanted, text)
		}
	}
}

func TestExportSelectorRangeRendersChronologicalSegments(t *testing.T) {
	t.Parallel()
	idx, record, _ := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			IncludeSelector: "0..2",
			FullHistory:     false,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"Compaction Segments 0..2",
		"first summary context",
		"micro context",
		"latest summary context",
		"tail after first",
		"tail after micro",
		"tail after latest",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("range export missing %q in:\n%s", want, text)
		}
	}
	first := strings.Index(text, "tail after first")
	micro := strings.Index(text, "tail after micro")
	latest := strings.Index(text, "tail after latest")
	if first < 0 || micro < 0 || latest < 0 || !(first < micro && micro < latest) {
		t.Fatalf("range export order first=%d micro=%d latest=%d in:\n%s", first, micro, latest, text)
	}
}

func TestExportFullHistorySelectsAllSegments(t *testing.T) {
	t.Parallel()
	idx, record, _ := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatJSON,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			IncludeSelector: "",
			FullHistory:     true,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	var document shapedCompactionJSONExport
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("unmarshal export body: %v", err)
	}
	if document.Compaction == nil {
		t.Fatal("compaction block was nil")
	}
	if document.Compaction.Selector != "all" {
		t.Fatalf("selector = %q, want all", document.Compaction.Selector)
	}
	if len(document.Compaction.Segments) != 3 {
		t.Fatalf("compaction segments = %d, want 3 starting summaries", len(document.Compaction.Segments))
	}
	if len(document.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 visible messages", len(document.Messages))
	}
}

func TestExportLastNPreservesStartingSummary(t *testing.T) {
	t.Parallel()
	idx, record, _ := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		LastN:        2,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			IncludeSelector: "0..2",
			FullHistory:     false,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	text := string(body)
	for _, want := range []string{"micro context", "latest summary context", "tail after micro", "tail after latest"} {
		if !strings.Contains(text, want) {
			t.Fatalf("last-n export missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "tail after first") {
		t.Fatalf("last-n export included older message in:\n%s", text)
	}
}

func TestExportRejectsOutOfRangeSegment(t *testing.T) {
	t.Parallel()
	idx, record, _ := newCompactionExportIndex()

	_, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			IncludeSelector: "4",
			FullHistory:     false,
		},
	})

	if err == nil {
		t.Fatal("expected out-of-range segment error")
	}
	if !strings.Contains(err.Error(), "conversation has compaction segments 0..3") {
		t.Fatalf("error = %q, want segment range", err.Error())
	}
}

func TestExportDefaultedSegmentWithoutCompactionRendersConversation(t *testing.T) {
	t.Parallel()
	idx, record := newPlainExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "plain visible message") {
		t.Fatalf("defaulted export = %q, want visible transcript message", text)
	}
	if strings.Contains(text, "Compaction Segments") {
		t.Fatalf("uncompacted export rendered compaction block: %q", text)
	}
}

func newCompactionExportIndex() (*Index, Record, *[]LoadOptions) {
	loadOptions := make([]LoadOptions, 0)
	registry := NewRegistry()
	registry.Register(&streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
		loadOptions = append(loadOptions, opts)
		return func(yield func(transcript.Message, error) bool) {
			for _, message := range compactionExportMessages() {
				if message.Role == "system" && !opts.IncludeSystemMessages {
					continue
				}
				if !yield(message, nil) {
					return
				}
			}
		}
	}})
	idx := newTestIndex(registry)
	idx.records = []Record{compactionExportRecord()}
	idx.loaded = true
	record := idx.records[0]
	return idx, record, &loadOptions
}

func newPlainExportIndex() (*Index, Record) {
	registry := NewRegistry()
	registry.Register(&streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
		return func(yield func(transcript.Message, error) bool) {
			if !yield(exportChatMessage("plain-1", "user", "plain visible message", 1), nil) {
				return
			}
		}
	}})
	idx := newTestIndex(registry)
	idx.records = []Record{compactionExportRecord()}
	idx.loaded = true
	record := idx.records[0]
	return idx, record
}

func compactionExportRecord() Record {
	return Record{
		ID:            "claude:compaction-export",
		Provider:      providerid.ProviderClaude,
		NativeID:      "compaction-export",
		Title:         "compaction-export",
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/compaction-export.jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(8, 0),
		SizeBytes:     10,
		Archived:      false,
	}
}

func compactionExportMessages() []transcript.Message {
	return []transcript.Message{
		exportBoundaryMessage(
			"boundary-1",
			transcript.CompactionKindBoundary,
			"first boundary text",
			[]transcript.CompactedContextItem{
				exportOrdinaryContextItem("first boundary context"),
			},
			1,
		),
		exportSummaryMessage(
			"summary-1",
			"boundary-1",
			"first summary transcript",
			[]transcript.CompactedContextItem{
				exportSummaryContextItem("first summary context"),
			},
			2,
		),
		exportChatMessage("tail-1", "user", "tail after first", 3),
		exportBoundaryMessage(
			"micro-2",
			transcript.CompactionKindMicroboundary,
			"micro boundary text",
			[]transcript.CompactedContextItem{
				exportOrdinaryContextItem("micro context"),
			},
			4,
		),
		exportChatMessage("tail-2", "assistant", "tail after micro", 5),
		exportBoundaryMessage(
			"boundary-3",
			transcript.CompactionKindBoundary,
			"latest boundary text",
			[]transcript.CompactedContextItem{
				exportOrdinaryContextItem("latest boundary context"),
				exportEncryptedReasoningContextItem(),
			},
			6,
		),
		exportSummaryMessage(
			"summary-3",
			"boundary-3",
			"latest summary transcript",
			[]transcript.CompactedContextItem{
				exportSummaryContextItem("latest summary context"),
			},
			7,
		),
		exportChatMessage("tail-3", "assistant", "tail after latest", 8),
	}
}

func exportBoundaryMessage(
	uuid string,
	kind transcript.CompactionKind,
	text string,
	contextItems []transcript.CompactedContextItem,
	unixTime int64,
) transcript.Message {
	return transcript.Message{
		UUID:              uuid,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "system",
		Visibility:        transcript.MessageVisibilityMetaOnly,
		Compaction: &transcript.CompactionMetadata{
			Kind:                      kind,
			Trigger:                   transcript.CompactionTriggerManual,
			PreTokens:                 100,
			PostTokens:                25,
			TokensSaved:               75,
			MessagesSummarized:        3,
			ReplacementHistoryCount:   len(contextItems),
			HeadUUID:                  "head-" + uuid,
			AnchorUUID:                "anchor-" + uuid,
			TailUUID:                  "tail-" + uuid,
			ContextItems:              contextItems,
			UserContext:               "",
			Direction:                 "",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        nil,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		},
		Timestamp: time.Unix(unixTime, 0),
		Text:      text,
		Thinking:  "",
		HasTools:  false,
		Tools:     nil,
	}
}

func exportSummaryMessage(
	uuid string,
	parentUUID string,
	text string,
	contextItems []transcript.CompactedContextItem,
	unixTime int64,
) transcript.Message {
	return transcript.Message{
		UUID:              uuid,
		ParentUUID:        parentUUID,
		LogicalParentUUID: "",
		Role:              "user",
		Visibility:        transcript.MessageVisibilityTranscriptOnly,
		Compaction: &transcript.CompactionMetadata{
			Kind:                      transcript.CompactionKindSummary,
			Trigger:                   transcript.CompactionTriggerUnknown,
			PreTokens:                 0,
			PostTokens:                0,
			TokensSaved:               0,
			MessagesSummarized:        2,
			ReplacementHistoryCount:   0,
			HeadUUID:                  "",
			AnchorUUID:                "",
			TailUUID:                  "",
			ContextItems:              contextItems,
			UserContext:               "",
			Direction:                 "",
			PreCompactDiscoveredTools: nil,
			CompactedToolIDs:          nil,
			ClearedAttachmentUUIDs:    nil,
			RawCompactMetadata:        nil,
			RawMicrocompactMetadata:   nil,
			RawSummarizeMetadata:      nil,
		},
		Timestamp: time.Unix(unixTime, 0),
		Text:      text,
		Thinking:  "",
		HasTools:  false,
		Tools:     nil,
	}
}

func exportChatMessage(uuid string, role string, text string, unixTime int64) transcript.Message {
	return transcript.Message{
		UUID:              uuid,
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              role,
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Unix(unixTime, 0),
		Text:              text,
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
}

func exportSummaryContextItem(text string) transcript.CompactedContextItem {
	return exportMessageContextItem(text, transcript.CompactedMessageClassSummary)
}

func exportOrdinaryContextItem(text string) transcript.CompactedContextItem {
	return exportMessageContextItem(text, transcript.CompactedMessageClassOrdinary)
}

func exportMessageContextItem(
	text string,
	messageClass transcript.CompactedMessageClass,
) transcript.CompactedContextItem {
	messageItem := transcript.CompactedMessageItem{
		Role:         "user",
		Phase:        "",
		Content:      []transcript.CompactedMessageContentItem{{Type: "text", Text: text, Raw: json.RawMessage(`"text"`)}},
		ContentRaw:   json.RawMessage(`[{"type":"text","text":"text"}]`),
		MessageClass: messageClass,
		Raw:          json.RawMessage(`{"type":"message","role":"user"}`),
	}
	return transcript.CompactedContextItem{
		Kind:                 transcript.CompactedContextItemKindMessage,
		Message:              &messageItem,
		Reasoning:            nil,
		LocalShellCall:       nil,
		FunctionCall:         nil,
		ToolSearchCall:       nil,
		FunctionCallOutput:   nil,
		CustomToolCall:       nil,
		CustomToolCallOutput: nil,
		ToolSearchOutput:     nil,
		WebSearchCall:        nil,
		ImageGenerationCall:  nil,
		Compaction:           nil,
		CompactionTrigger:    nil,
		ContextCompaction:    nil,
		Other:                nil,
	}
}
