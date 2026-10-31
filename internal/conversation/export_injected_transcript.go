package conversation

import (
	"encoding/json"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/reorienttag"
	"goodkind.io/clyde/internal/transcript"
)

const (
	injectedChatContentType     = "text"
	injectedThinkingSummaryType = "thinking"
	// A content filtered export returns this error when a compact summary
	// contains a transcript wrapper outside every resolved span.
	unresolvedInjectedTranscriptError = `conversation %s: cannot filter the pre-compaction transcript in compact summary %s by content kind; export all content kinds with --only all or MCP only: ["all"]`
)

// injectedTranscriptResolver returns typed transcript spans in one summary
// content item. It returns a span only when it establishes the kind of every
// segment in that span.
type injectedTranscriptResolver func(content transcript.CompactedMessageContentItem) []reorienttag.TranscriptSpan

// injectedTranscriptResolvers lists resolvers in the order export tries them.
// The first reads framed transcripts in the text; the second returns spans
// that the provider parser attached to the content item.
func injectedTranscriptResolvers() []injectedTranscriptResolver {
	return []injectedTranscriptResolver{resolveFramedTranscriptSpans, resolveParsedTranscriptSpans}
}

func resolveFramedTranscriptSpans(content transcript.CompactedMessageContentItem) []reorienttag.TranscriptSpan {
	return reorienttag.ParseTranscriptSpans(content.Text)
}

// resolveParsedTranscriptSpans returns the spans that the provider parser
// attached to the content item.
func resolveParsedTranscriptSpans(content transcript.CompactedMessageContentItem) []reorienttag.TranscriptSpan {
	return content.TranscriptSpans
}

// resolveInjectedTranscriptSpans returns spans from the first resolver that
// finds any in the content item.
func resolveInjectedTranscriptSpans(content transcript.CompactedMessageContentItem) []reorienttag.TranscriptSpan {
	for _, resolver := range injectedTranscriptResolvers() {
		if spans := resolver(content); len(spans) > 0 {
			return spans
		}
	}
	return nil
}

// unresolvedInjectedTranscriptSummary returns the UUID of the first starting
// summary in the selected segments that contains a transcript wrapper outside
// every resolved span. It also reports whether it found one.
func unresolvedInjectedTranscriptSummary(selection CompactionSegmentSelection) (string, bool) {
	for _, segment := range selection.Segments {
		if !segment.HasStartingSummary {
			continue
		}
		for _, item := range segment.Checkpoint.ContextItems {
			if item.Kind != transcript.CompactedContextItemKindMessage ||
				item.Message == nil ||
				item.Message.MessageClass != transcript.CompactedMessageClassSummary {
				continue
			}
			if slices.ContainsFunc(item.Message.Content, unresolvedInjectedTranscript) {
				return segment.SummaryUUID, true
			}
		}
	}
	return "", false
}

// unresolvedInjectedTranscript reports whether one summary content item's text
// contains a transcript wrapper start after removal of the resolved spans.
func unresolvedInjectedTranscript(content transcript.CompactedMessageContentItem) bool {
	var outside strings.Builder
	previousEnd := 0
	for _, span := range resolveInjectedTranscriptSpans(content) {
		outside.WriteString(content.Text[previousEnd:span.Start])
		previousEnd = span.End
	}
	outside.WriteString(content.Text[previousEnd:])
	return reorienttag.ContainsTranscriptWrapper(outside.String())
}

// expandInjectedTranscriptItems replaces each summary containing resolved spans
// with summary items for surrounding prose and a typed context item for each
// segment. The content kind filter selects those items by the rules for saved
// Codex context items.
func expandInjectedTranscriptItems(
	items []transcript.CompactedContextItem,
) []transcript.CompactedContextItem {
	expanded := make([]transcript.CompactedContextItem, 0, len(items))
	for _, item := range items {
		expanded = append(expanded, expandInjectedTranscriptItem(item)...)
	}
	return expanded
}

func expandInjectedTranscriptItem(
	item transcript.CompactedContextItem,
) []transcript.CompactedContextItem {
	if item.Kind != transcript.CompactedContextItemKindMessage ||
		item.Message == nil ||
		item.Message.MessageClass != transcript.CompactedMessageClassSummary {
		return []transcript.CompactedContextItem{item}
	}
	expanded := make([]transcript.CompactedContextItem, 0)
	prose := make([]transcript.CompactedMessageContentItem, 0)
	resolved := false
	for _, contentItem := range item.Message.Content {
		spans := resolveInjectedTranscriptSpans(contentItem)
		if len(spans) == 0 {
			prose = append(prose, contentItem)
			continue
		}
		resolved = true
		previousEnd := 0
		for _, span := range spans {
			prose = appendSummaryProse(prose, contentItem, contentItem.Text[previousEnd:span.Start])
			expanded = appendSummaryItem(expanded, *item.Message, prose)
			prose = make([]transcript.CompactedMessageContentItem, 0)
			for _, segment := range span.Segments {
				expanded = append(expanded, injectedSegmentItem(segment))
			}
			previousEnd = span.End
		}
		prose = appendSummaryProse(prose, contentItem, contentItem.Text[previousEnd:])
	}
	if !resolved {
		return []transcript.CompactedContextItem{item}
	}
	return appendSummaryItem(expanded, *item.Message, prose)
}

func appendSummaryProse(
	prose []transcript.CompactedMessageContentItem,
	source transcript.CompactedMessageContentItem,
	text string,
) []transcript.CompactedMessageContentItem {
	if strings.TrimSpace(text) == "" {
		return prose
	}
	source.Text = text
	source.Raw = nil
	source.TranscriptSpans = nil
	return append(prose, source)
}

func appendSummaryItem(
	items []transcript.CompactedContextItem,
	message transcript.CompactedMessageItem,
	prose []transcript.CompactedMessageContentItem,
) []transcript.CompactedContextItem {
	if len(prose) == 0 {
		return items
	}
	message.Content = prose
	message.ContentRaw = nil
	message.Raw = nil
	item := emptyInjectedContextItem(transcript.CompactedContextItemKindMessage)
	item.Message = &message
	return append(items, item)
}

// injectedSegmentItem converts thinking, tool calls, and tool outputs to
// reasoning, function call, and function call output items. It converts system
// text to a system message, chat to a message with the segment role, and other
// to an other item.
func injectedSegmentItem(segment reorienttag.Segment) transcript.CompactedContextItem {
	switch segment.Kind {
	case reorienttag.SegmentKindThinking:
		item := emptyInjectedContextItem(transcript.CompactedContextItemKindReasoning)
		item.Reasoning = &transcript.CompactedReasoningItem{
			Summary: []transcript.CompactedReasoningSummary{
				{Type: injectedThinkingSummaryType, Text: segment.Text, Raw: nil},
			},
			SummaryRaw:       nil,
			ContentRaw:       nil,
			EncryptedContent: "",
			Raw:              nil,
		}
		return item
	case reorienttag.SegmentKindToolCall:
		item := emptyInjectedContextItem(transcript.CompactedContextItemKindFunctionCall)
		item.FunctionCall = &transcript.CompactedFunctionCallItem{
			Name:      "",
			Namespace: "",
			Arguments: segment.Text,
			CallID:    "",
			Raw:       nil,
		}
		return item
	case reorienttag.SegmentKindToolOutput:
		item := emptyInjectedContextItem(transcript.CompactedContextItemKindFunctionCallOutput)
		item.FunctionCallOutput = &transcript.CompactedFunctionCallOutputItem{
			CallID:    "",
			OutputRaw: injectedOutputRaw(segment.Text),
			Raw:       nil,
		}
		return item
	case reorienttag.SegmentKindSystem:
		return injectedMessageItem(compactedRoleSystem, segment.Text)
	case reorienttag.SegmentKindChat:
		return injectedMessageItem(string(segment.Role), segment.Text)
	case reorienttag.SegmentKindOther:
		return injectedOtherItem(segment.Text)
	}
	return injectedOtherItem(segment.Text)
}

// injectedOtherItem builds an other context item with the segment text as a
// JSON string. The content kind filter selects other items only with
// raw_json_metadata.
func injectedOtherItem(text string) transcript.CompactedContextItem {
	item := emptyInjectedContextItem(transcript.CompactedContextItemKindOther)
	item.Other = &transcript.CompactedOtherItem{
		Type: string(reorienttag.SegmentKindOther),
		Raw:  injectedOutputRaw(text),
	}
	return item
}

func injectedMessageItem(role string, text string) transcript.CompactedContextItem {
	item := emptyInjectedContextItem(transcript.CompactedContextItemKindMessage)
	item.Message = &transcript.CompactedMessageItem{
		Role:  role,
		Phase: "",
		Content: []transcript.CompactedMessageContentItem{
			{Type: injectedChatContentType, Text: text, ImageURL: "", Detail: "", Raw: nil, TranscriptSpans: nil},
		},
		ContentRaw:   nil,
		MessageClass: transcript.CompactedMessageClassOrdinary,
		Raw:          nil,
	}
	return item
}

// injectedOutputRaw encodes segment text as a JSON string.
func injectedOutputRaw(text string) json.RawMessage {
	encoded, err := json.Marshal(text)
	if err != nil {
		return nil
	}
	return encoded
}

func emptyInjectedContextItem(kind transcript.CompactedContextItemKind) transcript.CompactedContextItem {
	return transcript.CompactedContextItem{
		Kind:                 kind,
		Message:              nil,
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

// filterInjectedTranscriptText keeps selected segments in resolved spans and
// preserves surrounding prose. It resolves spans from the summary content item
// in items with the same text, including any spans the parser attached to that
// item. It keeps a tool call frame with empty text unless the content filter
// selects tool_calls or tool_outputs.
func filterInjectedTranscriptText(
	text string,
	items []transcript.CompactedContextItem,
	content ContentKindSet,
) string {
	spans := resolveInjectedTranscriptSpans(summaryContentItemForText(text, items))
	if len(spans) == 0 {
		return text
	}
	var builder strings.Builder
	previousEnd := 0
	for _, span := range spans {
		builder.WriteString(text[previousEnd:span.Start])
		builder.WriteString(reorienttag.FramedTranscript(selectedInjectedSegments(span.Segments, content)))
		previousEnd = span.End
	}
	builder.WriteString(text[previousEnd:])
	return builder.String()
}

// summaryContentItemForText returns the summary content item in items with
// exactly this text. Without a match, it returns a text item without spans.
func summaryContentItemForText(
	text string,
	items []transcript.CompactedContextItem,
) transcript.CompactedMessageContentItem {
	for _, item := range items {
		if item.Kind != transcript.CompactedContextItemKindMessage ||
			item.Message == nil ||
			item.Message.MessageClass != transcript.CompactedMessageClassSummary {
			continue
		}
		for _, contentItem := range item.Message.Content {
			if contentItem.Text == text {
				return contentItem
			}
		}
	}
	return transcript.CompactedMessageContentItem{
		Type:            injectedChatContentType,
		Text:            text,
		ImageURL:        "",
		Detail:          "",
		Raw:             nil,
		TranscriptSpans: nil,
	}
}

func selectedInjectedSegments(
	segments []reorienttag.Segment,
	content ContentKindSet,
) []reorienttag.Segment {
	kept := make([]reorienttag.Segment, 0, len(segments))
	toolDetail := content.Has(ContentKindToolCalls) || content.Has(ContentKindToolOutputs)
	for _, segment := range segments {
		if !compactedItemSelected(injectedSegmentItem(segment), content) {
			continue
		}
		if segment.Kind == reorienttag.SegmentKindToolCall && !toolDetail {
			segment.Text = ""
		}
		kept = append(kept, segment)
	}
	return kept
}
