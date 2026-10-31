package conversation

import "goodkind.io/clyde/internal/transcript"

// exportShapeOptions builds renderer shaping options for the requested content kinds.
// It includes thinking when requested, omits tool turns when no tool kind is
// requested, and selects full tool detail or tool input summaries by content kind.
func exportShapeOptions(content ContentKindSet) transcript.ShapeOptions {
	includeTools := hasToolContent(content)
	shapeOptions := transcript.ShapeOptions{
		IncludeThinking:  content.Has(ContentKindThinking),
		ConversationOnly: !includeTools,
		MaxTextRunes:     0,
		ToolOnly:         transcript.ToolOnlyOmit,
	}
	if includeTools {
		shapeOptions.ToolOnly = exportToolOnlyMode(content)
	}
	return shapeOptions
}

// messageRendersForExport reports whether the content filter and shaping retain a
// message, ignoring history_start. Raw JSON metadata exports use the filter alone.
func messageRendersForExport(message transcript.Message, options ExportOptions) bool {
	options.HistoryStart = 0
	kept := filterMessages([]transcript.Message{message}, options)
	if len(kept) == 0 {
		return false
	}
	if options.Format == ExportFormatJSON && options.Content.Has(ContentKindRawJSONMetadata) {
		return true
	}
	return len(transcript.ShapeConversation(kept, exportShapeOptions(options.Content))) > 0
}
