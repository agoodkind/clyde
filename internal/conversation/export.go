package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// Export renders one raw conversation artifact. It collects every message
// because the rendered document spans the whole conversation.
func (idx *Index) Export(record Record, options ExportOptions) ([]byte, error) {
	messages, err := idx.LoadMessagesWithOptions(record, LoadOptions{
		IncludeSystemPrompts:  options.Content.Has(ContentKindSystemPrompts),
		IncludeSystemMessages: true,
		IncludeToolOutputs:    options.Content.Has(ContentKindToolOutputs),
	})
	if err != nil {
		return nil, err
	}
	return exportMessages(record, messages, options)
}

func exportMessages(record Record, messages []transcript.Message, options ExportOptions) ([]byte, error) {
	compactionOptions, err := NormalizeCompactionExportOptions(
		options.Compaction,
		options.HistoryStart,
		options.LastN,
	)
	if err != nil {
		return nil, err
	}
	options.Compaction = compactionOptions
	segments := CompactionSegments(messages)
	selection, err := SelectCompactionSegments(
		segments,
		options.Compaction.IncludeSelector,
	)
	if err != nil {
		return nil, err
	}
	selection = applyLastNToSegmentSelection(messages, selection, options.LastN)
	selectedMessages := selectedCompactionSegmentMessages(messages, selection)
	selectedMessages = filterMessages(selectedMessages, options)
	if options.Format == ExportFormatJSON && !options.Content.Has(ContentKindRawJSONMetadata) {
		clearMetadata(selectedMessages)
	}
	compactionBlock := buildCompactionExportBlock(selection)
	body, err := renderMessagesWithCompaction(record, selectedMessages, options, compactionBlock)
	if err != nil {
		return nil, err
	}
	body = compressWhitespace(body, options.Format, options.Whitespace)
	if options.MaxLines > 0 {
		capped, _, _ := capToLastLines(string(body), options.MaxLines)
		body = []byte(capped)
	}
	return body, nil
}

// capToLastLines keeps only the last maxLines lines of text. It returns the
// capped text, the line count of the returned text, and whether earlier lines
// were dropped. A maxLines of zero or below leaves the text unchanged. The
// returned text ends with a trailing newline when it was capped so the last
// line is complete.
func capToLastLines(text string, maxLines int) (string, int, bool) {
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return text, 0, false
	}
	total := strings.Count(trimmed, "\n") + 1
	if maxLines <= 0 || total <= maxLines {
		return text, total, false
	}
	// Scan backward for the newline before the first kept line, so a large
	// transcript is not split into one slice entry per line.
	cut := len(trimmed)
	for range maxLines {
		idx := strings.LastIndexByte(trimmed[:cut], '\n')
		if idx < 0 {
			cut = 0
			break
		}
		cut = idx
	}
	start := cut
	if start > 0 {
		start++
	}
	return trimmed[start:] + "\n", maxLines, true
}

type rawJSONExport struct {
	ConversationID        string                 `json:"conversation_id"`
	Provider              string                 `json:"provider"`
	ArtifactPath          string                 `json:"artifact_path"`
	Compaction            *compactionExportBlock `json:"compaction,omitempty"`
	Messages              []transcript.Message   `json:"messages"`
	CompactionCheckpoints []CompactionCheckpoint `json:"compaction_checkpoints,omitempty"`
}

func renderMessagesWithCompaction(
	record Record,
	messages []transcript.Message,
	options ExportOptions,
	compactionBlock *compactionExportBlock,
) ([]byte, error) {
	includeTools := hasToolContent(options.Content)
	shapeOptions := transcript.ShapeOptions{
		IncludeThinking:  options.Content.Has(ContentKindThinking),
		ConversationOnly: !includeTools,
		MaxTextRunes:     0,
		ToolOnly:         transcript.ToolOnlyOmit,
	}
	if includeTools {
		shapeOptions.ToolOnly = exportToolOnlyMode(options.Content)
	}
	switch options.Format {
	case ExportFormatHTML:
		body := []byte(transcript.RenderHTMLWithOptions(messages, shapeOptions))
		return prependCompactionBlock(body, options.Format, compactionBlock)
	case ExportFormatJSON:
		if options.Content.Has(ContentKindRawJSONMetadata) {
			return renderRawJSON(record, messages, options, compactionBlock)
		}
		if compactionBlock != nil {
			return renderCompactionJSON(record, messages, shapeOptions, compactionBlock)
		}
		body, err := transcript.RenderJSONWithOptions(messages, shapeOptions)
		if err != nil {
			slog.Warn("conversation.export.render_json_failed", "concern", "conversation.export", "component", "conversation", "err", err)
			return nil, fmt.Errorf("render json transcript: %w", err)
		}
		return body, nil
	case ExportFormatPlainText:
		body := []byte(transcript.RenderPlainTextWithOptions(messages, shapeOptions))
		return prependCompactionBlock(body, options.Format, compactionBlock)
	case ExportFormatMarkdown, "":
		body := []byte(transcript.RenderMarkdownWithOptions(messages, shapeOptions))
		return prependCompactionBlock(body, options.Format, compactionBlock)
	default:
		return nil, fmt.Errorf("unsupported export format %q", options.Format)
	}
}

func hasToolContent(content ContentKindSet) bool {
	return content.Has(ContentKindToolSummaries) ||
		content.Has(ContentKindToolCalls) ||
		content.Has(ContentKindToolOutputs)
}

func exportToolOnlyMode(content ContentKindSet) transcript.ToolOnlyMode {
	if content.Has(ContentKindToolCalls) || content.Has(ContentKindToolOutputs) {
		return transcript.ToolOnlyFullDetail
	}
	return transcript.ToolOnlyInputSummary
}

type shapedCompactionJSONExport struct {
	ConversationID string                        `json:"conversation_id"`
	Provider       string                        `json:"provider"`
	ArtifactPath   string                        `json:"artifact_path"`
	Compaction     *compactionExportBlock        `json:"compaction,omitempty"`
	Messages       []transcript.ConversationTurn `json:"messages"`
}

type compactionExportBlock struct {
	Selector string                         `json:"selector"`
	Segments []compactionExportSegmentBlock `json:"segments"`
}

type compactionExportSegmentBlock struct {
	SegmentIndex        int                               `json:"segment_index"`
	HasStartingSummary  bool                              `json:"has_starting_summary"`
	StartMessageIndex   int                               `json:"start_message_index"`
	EndMessageIndex     int                               `json:"end_message_index"`
	SummaryMessageIndex int                               `json:"summary_message_index"`
	SummaryUUID         string                            `json:"summary_uuid,omitempty"`
	SummaryTimestamp    *time.Time                        `json:"summary_timestamp,omitempty"`
	SummaryItems        []transcript.CompactedContextItem `json:"summary_items,omitempty"`
	ContextItems        []transcript.CompactedContextItem `json:"context_items,omitempty"`
}

func renderRawJSON(
	record Record,
	messages []transcript.Message,
	options ExportOptions,
	compactionBlock *compactionExportBlock,
) ([]byte, error) {
	document := rawJSONExport{
		ConversationID:        record.ID,
		Provider:              record.Provider.String(),
		ArtifactPath:          record.ArtifactPath,
		Compaction:            compactionBlock,
		Messages:              messages,
		CompactionCheckpoints: nil,
	}
	if options.Content.Has(ContentKindSystemMessages) {
		document.CompactionCheckpoints = CompactionCheckpoints(messages)
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		slog.Warn("conversation.export.render_raw_json_failed", "concern", "conversation.export", "component", "conversation", "err", err)
		return nil, fmt.Errorf("render raw json transcript: %w", err)
	}
	return append(body, '\n'), nil
}

func applyLastNToSegmentSelection(
	messages []transcript.Message,
	selection CompactionSegmentSelection,
	lastN int,
) CompactionSegmentSelection {
	if lastN <= 0 {
		return selection
	}
	remaining := lastN
	cutoffSelectionIndex := 0
	cutoffMessageIndex := -1
	for i, segment := range slices.Backward(selection.Segments) {
		for messageIndex := min(segment.EndMessageIndex, len(messages)) - 1; messageIndex >= segment.StartMessageIndex; messageIndex-- {
			if !messageCountsForLastN(messages[messageIndex]) {
				continue
			}
			remaining--
			if remaining == 0 {
				cutoffSelectionIndex = i
				cutoffMessageIndex = messageIndex
				break
			}
		}
		if cutoffMessageIndex >= 0 {
			break
		}
	}
	if cutoffMessageIndex < 0 {
		return selection
	}
	out := make([]CompactionSegment, 0, len(selection.Segments)-cutoffSelectionIndex)
	for i := cutoffSelectionIndex; i < len(selection.Segments); i++ {
		segment := selection.Segments[i]
		if i == cutoffSelectionIndex {
			segment.StartMessageIndex = cutoffMessageIndex
		}
		out = append(out, segment)
	}
	return CompactionSegmentSelection{
		Selector: selection.Selector,
		Segments: out,
	}
}

func messageCountsForLastN(message transcript.Message) bool {
	if message.Compaction != nil {
		return false
	}
	return message.Visibility != transcript.MessageVisibilityMetaOnly
}

func selectedCompactionSegmentMessages(
	messages []transcript.Message,
	selection CompactionSegmentSelection,
) []transcript.Message {
	selected := make([]transcript.Message, 0)
	for _, segment := range selection.Segments {
		start := max(segment.StartMessageIndex, 0)
		end := min(segment.EndMessageIndex, len(messages))
		if start >= end {
			continue
		}
		selected = append(selected, messages[start:end]...)
	}
	return selected
}

func buildCompactionExportBlock(selection CompactionSegmentSelection) *compactionExportBlock {
	segmentBlocks := make([]compactionExportSegmentBlock, 0, len(selection.Segments))
	for _, segment := range selection.Segments {
		if !segment.HasStartingSummary {
			continue
		}
		var summaryTimestamp *time.Time
		if !segment.SummaryTimestamp.IsZero() {
			timestamp := segment.SummaryTimestamp
			summaryTimestamp = &timestamp
		}
		segmentBlocks = append(segmentBlocks, compactionExportSegmentBlock{
			SegmentIndex:        segment.Index,
			HasStartingSummary:  segment.HasStartingSummary,
			StartMessageIndex:   segment.StartMessageIndex,
			EndMessageIndex:     segment.EndMessageIndex,
			SummaryMessageIndex: segment.SummaryMessageIndex,
			SummaryUUID:         segment.SummaryUUID,
			SummaryTimestamp:    summaryTimestamp,
			SummaryItems:        summaryContextItems(segment.Checkpoint.ContextItems),
			ContextItems:        copyContextItems(segment.Checkpoint.ContextItems),
		})
	}
	if len(segmentBlocks) == 0 {
		return nil
	}
	return &compactionExportBlock{
		Selector: selection.Selector,
		Segments: segmentBlocks,
	}
}

func summaryContextItems(
	items []transcript.CompactedContextItem,
) []transcript.CompactedContextItem {
	summaryItems := make([]transcript.CompactedContextItem, 0, len(items))
	for _, item := range items {
		if item.Kind != transcript.CompactedContextItemKindMessage ||
			item.Message == nil ||
			item.Message.MessageClass != transcript.CompactedMessageClassSummary {
			continue
		}
		summaryItems = append(summaryItems, item)
	}
	return summaryItems
}

func renderCompactionJSON(
	record Record,
	messages []transcript.Message,
	shapeOptions transcript.ShapeOptions,
	compactionBlock *compactionExportBlock,
) ([]byte, error) {
	document := shapedCompactionJSONExport{
		ConversationID: record.ID,
		Provider:       record.Provider.String(),
		ArtifactPath:   record.ArtifactPath,
		Compaction:     compactionBlock,
		Messages:       transcript.ShapeConversation(messages, shapeOptions),
	}
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		slog.Warn("conversation.export.render_compaction_json_failed", "concern", "conversation.export", "component", "conversation", "err", err)
		return nil, fmt.Errorf("render compaction JSON transcript: %w", err)
	}
	return append(body, '\n'), nil
}

func prependCompactionBlock(
	body []byte,
	format ExportFormat,
	compactionBlock *compactionExportBlock,
) ([]byte, error) {
	if compactionBlock == nil {
		return body, nil
	}
	prefix, err := renderCompactionBlock(compactionBlock, format)
	if err != nil {
		return nil, err
	}
	trimmedBody := bytes.TrimSpace(body)
	if len(trimmedBody) == 0 {
		return prefix, nil
	}
	separator := []byte("\n\n")
	if format == ExportFormatHTML {
		separator = []byte("\n")
	}
	out := make([]byte, 0, len(prefix)+len(separator)+len(trimmedBody))
	out = append(out, prefix...)
	out = append(out, separator...)
	out = append(out, trimmedBody...)
	return out, nil
}

func renderCompactionBlock(
	compactionBlock *compactionExportBlock,
	format ExportFormat,
) ([]byte, error) {
	body, err := json.MarshalIndent(compactionBlock, "", "  ")
	if err != nil {
		slog.Warn("conversation.export.render_compaction_block_failed", "concern", "conversation.export", "component", "conversation", "err", err)
		return nil, fmt.Errorf("render compaction block: %w", err)
	}
	title := compactionBlockTitle(compactionBlock)
	switch format {
	case ExportFormatHTML:
		var out strings.Builder
		out.WriteString("<section class=\"compaction\">\n")
		out.WriteString("<h2>" + html.EscapeString(title) + "</h2>\n")
		out.WriteString("<pre>" + html.EscapeString(string(body)) + "</pre>\n")
		out.WriteString("</section>")
		return []byte(out.String()), nil
	case ExportFormatJSON:
		return body, nil
	case ExportFormatPlainText:
		return []byte(title + "\n\n" + string(body)), nil
	case ExportFormatMarkdown, "":
		return []byte("## " + title + "\n\n```json\n" + string(body) + "\n```"), nil
	default:
		return []byte("## " + title + "\n\n```json\n" + string(body) + "\n```"), nil
	}
}

func compactionBlockTitle(compactionBlock *compactionExportBlock) string {
	return "Compaction Segments " + compactionBlock.Selector
}

func filterMessages(messages []transcript.Message, options ExportOptions) []transcript.Message {
	out := make([]transcript.Message, 0, len(messages))
	for i, message := range messages {
		if options.HistoryStart > 0 && i < options.HistoryStart {
			continue
		}
		if message.Compaction != nil && !options.Content.Has(ContentKindSystemMessages) {
			continue
		}
		if !options.Content.Has(ContentKindChat) && message.Text != "" {
			message.Text = ""
		}
		if !options.Content.Has(ContentKindThinking) {
			message.Thinking = ""
		}
		if !hasToolContent(options.Content) {
			message.Tools = nil
			message.HasTools = false
		}
		if message.Compaction != nil && options.Content.Has(ContentKindSystemMessages) {
			out = append(out, message)
			continue
		}
		if message.Text == "" && message.Thinking == "" && len(message.Tools) == 0 {
			continue
		}
		out = append(out, message)
	}
	return out
}

func clearMetadata(messages []transcript.Message) {
	for i := range messages {
		messages[i].UUID = ""
	}
}

func compressWhitespace(body []byte, format ExportFormat, mode WhitespaceMode) []byte {
	if mode == "" || mode == WhitespacePreserve {
		return body
	}
	if format == ExportFormatJSON {
		var buf bytes.Buffer
		if err := json.Compact(&buf, body); err == nil {
			return buf.Bytes()
		}
		return body
	}
	return []byte(compressWhitespaceText(string(body), mode))
}

func compressWhitespaceText(text string, mode WhitespaceMode) string {
	if mode == WhitespacePreserve || mode == "" {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasPrefix(strings.TrimSpace(trimmed), "```") {
			inFence = !inFence
		}
		if inFence {
			out = append(out, line)
			continue
		}
		if strings.TrimSpace(trimmed) == "" {
			if mode == WhitespaceDense {
				continue
			}
			if mode == WhitespaceCompact {
				if blank {
					continue
				}
				blank = true
			}
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, trimmed)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}
