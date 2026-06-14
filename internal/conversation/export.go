package conversation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

// Export renders one raw conversation artifact. It collects every message
// because the rendered document spans the whole conversation.
func (idx *Index) Export(record Record, options ExportOptions) ([]byte, error) {
	compactionOptions, err := NormalizeCompactionExportOptions(
		options.Compaction,
		options.HistoryStart,
	)
	if err != nil {
		return nil, err
	}
	options.Compaction = compactionOptions
	compactionScoped := options.Compaction.Scope != CompactionExportScopeFull
	messages, err := idx.LoadMessagesWithOptions(record, LoadOptions{
		IncludeSystemPrompts:  options.Content.Has(ContentKindSystemPrompts),
		IncludeSystemMessages: options.Content.Has(ContentKindSystemMessages) || compactionScoped,
		IncludeToolOutputs:    options.Content.Has(ContentKindToolOutputs),
	})
	if err != nil {
		return nil, err
	}
	if compactionScoped {
		body, err := renderCompactionScopedExport(record, messages, options)
		if err != nil {
			return nil, err
		}
		return compressWhitespace(body, options.Format, options.Whitespace), nil
	}
	messages = filterMessages(messages, options)
	if options.Format == ExportFormatJSON && !options.Content.Has(ContentKindRawJSONMetadata) {
		clearMetadata(messages)
	}
	body, err := renderMessages(record, messages, options)
	if err != nil {
		return nil, err
	}
	return compressWhitespace(body, options.Format, options.Whitespace), nil
}

type rawJSONExport struct {
	ConversationID        string                 `json:"conversation_id"`
	Provider              string                 `json:"provider"`
	ArtifactPath          string                 `json:"artifact_path"`
	Compaction            *compactionExportBlock `json:"compaction,omitempty"`
	Messages              []transcript.Message   `json:"messages"`
	CompactionCheckpoints []CompactionCheckpoint `json:"compaction_checkpoints,omitempty"`
}

func renderMessages(record Record, messages []transcript.Message, options ExportOptions) ([]byte, error) {
	return renderMessagesWithCompaction(record, messages, options, nil)
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

type compactionExportSelection struct {
	Number     int
	Checkpoint CompactionCheckpoint
	TailStart  int
}

type compactionExportBlock struct {
	CheckpointNumber int                               `json:"checkpoint_number"`
	Scope            CompactionExportScope             `json:"scope"`
	Detail           CompactionExportDetail            `json:"detail"`
	Checkpoint       *CompactionCheckpoint             `json:"checkpoint,omitempty"`
	SummaryItems     []transcript.CompactedContextItem `json:"summary_items,omitempty"`
	ContextItems     []transcript.CompactedContextItem `json:"context_items,omitempty"`
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

func renderCompactionScopedExport(
	record Record,
	messages []transcript.Message,
	options ExportOptions,
) ([]byte, error) {
	selection, err := selectCompactionExportCheckpoint(
		CompactionCheckpoints(messages),
		options.Compaction,
	)
	if err != nil {
		return nil, err
	}
	tailStart := min(selection.TailStart, len(messages))
	tailMessages := filterMessages(messages[tailStart:], options)
	if options.Format == ExportFormatJSON && !options.Content.Has(ContentKindRawJSONMetadata) {
		clearMetadata(tailMessages)
	}
	compactionBlock := buildCompactionExportBlock(selection, options.Compaction)
	return renderMessagesWithCompaction(record, tailMessages, options, compactionBlock)
}

func selectCompactionExportCheckpoint(
	checkpoints []CompactionCheckpoint,
	options CompactionExportOptions,
) (compactionExportSelection, error) {
	switch options.Scope {
	case CompactionExportScopeCurrentContext:
		for i, checkpoint := range slices.Backward(checkpoints) {
			if checkpoint.HasUsableCompactedContext() {
				return compactionExportSelection{
					Number:     i + 1,
					Checkpoint: checkpoint,
					TailStart:  checkpointTailStart(checkpoint),
				}, nil
			}
		}
		return compactionExportSelection{}, fmt.Errorf("no compaction checkpoint with parsed context")
	case CompactionExportScopeFromCheckpoint:
		if options.CheckpointNumber < 1 || options.CheckpointNumber > len(checkpoints) {
			return compactionExportSelection{}, fmt.Errorf(
				"compaction checkpoint %d not found; conversation has %d checkpoints",
				options.CheckpointNumber,
				len(checkpoints),
			)
		}
		checkpoint := checkpoints[options.CheckpointNumber-1]
		return compactionExportSelection{
			Number:     options.CheckpointNumber,
			Checkpoint: checkpoint,
			TailStart:  checkpointTailStart(checkpoint),
		}, nil
	case CompactionExportScopeFull:
		fallthrough
	default:
		return compactionExportSelection{}, fmt.Errorf(
			"compaction scope %s does not select a checkpoint",
			options.Scope,
		)
	}
}

func checkpointTailStart(checkpoint CompactionCheckpoint) int {
	lastIndex := max(checkpoint.BoundaryIndex, checkpoint.SummaryIndex)
	if lastIndex < 0 {
		return 0
	}
	return lastIndex + 1
}

func buildCompactionExportBlock(
	selection compactionExportSelection,
	options CompactionExportOptions,
) *compactionExportBlock {
	switch options.Detail {
	case CompactionExportDetailNone:
		return nil
	case CompactionExportDetailSummary:
		return &compactionExportBlock{
			CheckpointNumber: selection.Number,
			Scope:            options.Scope,
			Detail:           options.Detail,
			Checkpoint:       nil,
			SummaryItems:     summaryContextItems(selection.Checkpoint.ContextItems),
			ContextItems:     nil,
		}
	case CompactionExportDetailContext:
		return &compactionExportBlock{
			CheckpointNumber: selection.Number,
			Scope:            options.Scope,
			Detail:           options.Detail,
			Checkpoint:       nil,
			SummaryItems:     nil,
			ContextItems:     copyContextItems(selection.Checkpoint.ContextItems),
		}
	case CompactionExportDetailFull:
		checkpoint := selection.Checkpoint
		return &compactionExportBlock{
			CheckpointNumber: selection.Number,
			Scope:            options.Scope,
			Detail:           options.Detail,
			Checkpoint:       &checkpoint,
			SummaryItems:     nil,
			ContextItems:     nil,
		}
	default:
		return nil
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
	var label string
	switch compactionBlock.Detail {
	case CompactionExportDetailSummary:
		label = "Summary"
	case CompactionExportDetailFull:
		label = "Full Details"
	case CompactionExportDetailNone, CompactionExportDetailContext:
		label = "Context"
	default:
		label = "Context"
	}
	return fmt.Sprintf("Compaction %d %s", compactionBlock.CheckpointNumber, label)
}

func filterMessages(messages []transcript.Message, options ExportOptions) []transcript.Message {
	out := make([]transcript.Message, 0, len(messages))
	compactionMessages := transcript.CompactionMessages(messages)
	compactionByMetadata := make(map[*transcript.CompactionMetadata]struct{}, len(compactionMessages))
	for _, message := range compactionMessages {
		if message.Compaction == nil {
			continue
		}
		compactionByMetadata[message.Compaction] = struct{}{}
	}
	for i, message := range messages {
		if options.HistoryStart > 0 && i < options.HistoryStart {
			continue
		}
		if message.Role == "system" &&
			message.Compaction != nil &&
			!options.Content.Has(ContentKindSystemMessages) {
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
		_, keepEmptyCompaction := compactionByMetadata[message.Compaction]
		if message.Text == "" && message.Thinking == "" && len(message.Tools) == 0 && !keepEmptyCompaction {
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
