package conversation

import (
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

func TestExportFullScopeDoesNotLoadSystemCompactionMessages(t *testing.T) {
	t.Parallel()
	idx, record, loadOptions := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatPlainText,
		HistoryStart: 0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			Scope:            CompactionExportScopeFull,
			Detail:           "",
			CheckpointNumber: 0,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	if len(*loadOptions) != 1 {
		t.Fatalf("load options calls = %d, want 1", len(*loadOptions))
	}
	if (*loadOptions)[0].IncludeSystemMessages {
		t.Fatalf("full export loaded system messages")
	}
	text := string(body)
	if strings.Contains(text, "boundary text") {
		t.Fatalf("full export leaked system boundary text: %q", text)
	}
	if !strings.Contains(text, "tail after latest") {
		t.Fatalf("full export text = %q, want visible tail", text)
	}
}

func TestExportDefaultUsesLatestCompactionFullDetail(t *testing.T) {
	t.Parallel()
	idx, record, loadOptions := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
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
		t.Fatalf("default current context export did not load system messages")
	}
	text := string(body)
	for _, want := range []string{
		"Compaction 3 Full Details",
		"latest boundary context",
		"latest summary context",
		"tail after latest",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("default current context export missing %q in:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"tail after first", "tail after micro", "latest boundary text"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("default current context export included %q in:\n%s", unwanted, text)
		}
	}
}

func TestExportCurrentContextUsesLatestUsableCompaction(t *testing.T) {
	t.Parallel()
	idx, record, loadOptions := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			Scope:            CompactionExportScopeCurrentContext,
			Detail:           "",
			CheckpointNumber: 0,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	if len(*loadOptions) != 1 {
		t.Fatalf("load options calls = %d, want 1", len(*loadOptions))
	}
	if !(*loadOptions)[0].IncludeSystemMessages {
		t.Fatalf("current context export did not load system messages")
	}
	text := string(body)
	for _, want := range []string{
		"Compaction 3 Full Details",
		"latest boundary context",
		"latest summary context",
		"tail after latest",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("current context export missing %q in:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"tail after first", "tail after micro", "latest boundary text"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("current context export included %q in:\n%s", unwanted, text)
		}
	}
}

func TestExportFromCheckpointRendersSummaryOnly(t *testing.T) {
	t.Parallel()
	idx, record, _ := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			Scope:            CompactionExportScopeFromCheckpoint,
			Detail:           CompactionExportDetailSummary,
			CheckpointNumber: 1,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		"Compaction 1 Summary",
		"first summary context",
		"tail after first",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("from-checkpoint export missing %q in:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"first boundary context", "micro boundary text"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("from-checkpoint export included %q in:\n%s", unwanted, text)
		}
	}
}

func TestExportFromCheckpointJSONIncludesFullParsedDetail(t *testing.T) {
	t.Parallel()
	idx, record, _ := newCompactionExportIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatJSON,
		HistoryStart: 0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			Scope:            CompactionExportScopeFromCheckpoint,
			Detail:           CompactionExportDetailFull,
			CheckpointNumber: 2,
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
	if document.Compaction.CheckpointNumber != 2 {
		t.Fatalf("checkpoint number = %d, want 2", document.Compaction.CheckpointNumber)
	}
	if document.Compaction.Checkpoint == nil ||
		document.Compaction.Checkpoint.BoundaryUUID != "micro-2" {
		t.Fatalf("checkpoint = %#v, want micro-2", document.Compaction.Checkpoint)
	}
	if len(document.Messages) == 0 || document.Messages[0].Text != "tail after micro" {
		t.Fatalf("messages = %#v, want tail after micro first", document.Messages)
	}
}

func TestExportCurrentContextFallsBackToFullWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
		Compaction: CompactionExportOptions{
			Scope:            CompactionExportScopeCurrentContext,
			Detail:           CompactionExportDetailContext,
			CheckpointNumber: 0,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "visible transcript message") {
		t.Fatalf("fallback export = %q, want visible transcript message", text)
	}
	if strings.Contains(text, "Conversation compacted") {
		t.Fatalf("fallback export leaked system message: %q", text)
	}
}

func TestExportDefaultedScopeFallsBackToFullWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatMarkdown,
		HistoryStart: 0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat),
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "visible transcript message") {
		t.Fatalf("defaulted export = %q, want visible transcript message", text)
	}
	if strings.Contains(text, "Conversation compacted") {
		t.Fatalf("defaulted export leaked system message: %q", text)
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
		Kind:    transcript.CompactedContextItemKindMessage,
		Message: &messageItem,
	}
}
