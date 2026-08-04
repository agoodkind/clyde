package conversation

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

func optionAwareStream(opts LoadOptions) []transcript.Message {
	return optionAwareStreamWithSystemText(opts, "Conversation compacted")
}

func optionAwareStreamWithSystemText(
	opts LoadOptions,
	systemText string,
) []transcript.Message {
	messages := make([]transcript.Message, 0, 2)
	if opts.IncludeSystemMessages {
		messages = append(messages, transcript.Message{
			UUID:              "system-1",
			ParentUUID:        "",
			LogicalParentUUID: "",
			Role:              "system",
			Visibility:        transcript.MessageVisibilityVisible,
			Compaction: &transcript.CompactionMetadata{
				Kind:                      transcript.CompactionKindBoundary,
				Trigger:                   transcript.CompactionTriggerManual,
				PreTokens:                 10,
				PostTokens:                5,
				TokensSaved:               5,
				MessagesSummarized:        0,
				ReplacementHistoryCount:   1,
				HeadUUID:                  "",
				AnchorUUID:                "",
				TailUUID:                  "",
				ContextItems:              nil,
				UserContext:               "",
				Direction:                 "",
				PreCompactDiscoveredTools: nil,
				CompactedToolIDs:          nil,
				ClearedAttachmentUUIDs:    nil,
				RawCompactMetadata:        nil,
				RawMicrocompactMetadata:   nil,
				RawSummarizeMetadata:      nil,
			},
			Timestamp: time.Unix(1, 0),
			Text:      systemText,
			Thinking:  "",
			HasTools:  false,
			Tools:     nil,
		})
	}
	messages = append(messages, transcript.Message{
		UUID:              "user-1",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "user",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Unix(2, 0),
		Text:              "visible transcript message",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	})
	return messages
}

func newOptionAwareIndex() (*Index, Record) {
	registry := NewRegistry()
	registry.Register(&streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
		return func(yield func(transcript.Message, error) bool) {
			for _, message := range optionAwareStream(opts) {
				if !yield(message, nil) {
					return
				}
			}
		}
	}})
	idx := newTestIndex(registry)
	idx.records = []Record{{
		ID:            "claude:system-opt-out",
		Provider:      ProviderClaude,
		NativeID:      "system-opt-out",
		Title:         "system-opt-out",
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/system-opt-out.jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}}
	idx.loaded = true
	record := idx.records[0]
	return idx, record
}

func newOptionAwareIndexWithSystemText(systemText string) (*Index, Record) {
	registry := NewRegistry()
	registry.Register(&streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
		return func(yield func(transcript.Message, error) bool) {
			for _, message := range optionAwareStreamWithSystemText(opts, systemText) {
				if !yield(message, nil) {
					return
				}
			}
		}
	}})
	idx := newTestIndex(registry)
	idx.records = []Record{{
		ID:            "claude:system-opt-out-empty",
		Provider:      ProviderClaude,
		NativeID:      "system-opt-out-empty",
		Title:         "system-opt-out-empty",
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/system-opt-out-empty.jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}}
	idx.loaded = true
	record := idx.records[0]
	return idx, record
}

type streamFuncParser struct {
	stream func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error]
}

func (*streamFuncParser) Provider() providerid.Provider { return providerid.ProviderClaude }

func (*streamFuncParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (*streamFuncParser) ScanRecord(string, FileStamp) (Record, bool) {
	return Record{}, false
}

func (p *streamFuncParser) Stream(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
	return p.stream(path, opts)
}

func TestRenderPlainTextDoesNotOptIntoSystemMessages(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	text, err := idx.RenderPlainText(record, 0)
	if err != nil {
		t.Fatalf("RenderPlainText returned error: %v", err)
	}
	if strings.Contains(text, "Conversation compacted") {
		t.Fatalf("rendered text unexpectedly included system message: %q", text)
	}
	if !strings.Contains(text, "visible transcript message") {
		t.Fatalf("rendered text = %q, want visible transcript message", text)
	}
}

func TestLoadMessagesDoesNotOptIntoSystemMessages(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	messages, err := idx.LoadMessages(record, false, false)
	if err != nil {
		t.Fatalf("LoadMessages returned error: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Text != "visible transcript message" {
		t.Fatalf("messages[0] = %#v", messages[0])
	}
}

func TestContextWindowTextDoesNotOptIntoSystemMessages(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	text, err := idx.ContextWindowText(record, "", 0, 0, 0, "")
	if err != nil {
		t.Fatalf("ContextWindowText returned error: %v", err)
	}
	if strings.Contains(text, "Conversation compacted") {
		t.Fatalf("context text unexpectedly included system message: %q", text)
	}
	if !strings.Contains(text, "visible transcript message") {
		t.Fatalf("context text = %q, want visible transcript message", text)
	}
}

func TestExportDoesNotOptIntoSystemMessages(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatPlainText,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat, ContentKindSystemPrompts),
		Compaction: CompactionExportOptions{
			IncludeSelector: "all",
			FullHistory:     false,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}
	text := string(body)
	if strings.Contains(text, "Conversation compacted") {
		t.Fatalf("export unexpectedly included system message: %q", text)
	}
	if !strings.Contains(text, "visible transcript message") {
		t.Fatalf("export text = %q, want visible transcript message", text)
	}
}

func TestExportJSONCanOptIntoSystemMessagesAndCheckpoints(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndex()

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatJSON,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindChat, ContentKindSystemMessages, ContentKindRawJSONMetadata),
		Compaction: CompactionExportOptions{
			IncludeSelector: "all",
			FullHistory:     false,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}

	var document struct {
		Messages    []transcript.Message   `json:"messages"`
		Checkpoints []CompactionCheckpoint `json:"compaction_checkpoints"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("unmarshal export body: %v", err)
	}
	if len(document.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(document.Messages))
	}
	if len(document.Checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(document.Checkpoints))
	}
	if document.Checkpoints[0].BoundaryUUID != "system-1" {
		t.Fatalf("checkpoint boundary uuid = %q, want system-1", document.Checkpoints[0].BoundaryUUID)
	}
}

func TestExportJSONRetainsEmptyCompactionBoundaryWhenSystemMessagesSelected(t *testing.T) {
	t.Parallel()
	idx, record := newOptionAwareIndexWithSystemText("")

	body, err := idx.Export(record, ExportOptions{
		Format:       ExportFormatJSON,
		HistoryStart: 0,
		LastN:        0,
		Whitespace:   WhitespacePreserve,
		Content:      NewContentKindSet(ContentKindSystemMessages, ContentKindRawJSONMetadata),
		Compaction: CompactionExportOptions{
			IncludeSelector: "all",
			FullHistory:     false,
		},
	})
	if err != nil {
		t.Fatalf("Export returned error: %v", err)
	}

	var document struct {
		Messages    []transcript.Message   `json:"messages"`
		Checkpoints []CompactionCheckpoint `json:"compaction_checkpoints"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("unmarshal export body: %v", err)
	}
	if len(document.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(document.Messages))
	}
	if document.Messages[0].UUID != "system-1" {
		t.Fatalf("message uuid = %q, want system-1", document.Messages[0].UUID)
	}
	if len(document.Checkpoints) != 1 {
		t.Fatalf("checkpoints len = %d, want 1", len(document.Checkpoints))
	}
}
