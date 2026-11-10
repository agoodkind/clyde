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
	liveTextPartSeparator       = "\n\n"
	// A content filtered export returns this error when a compact summary segment
	// contains a transcript wrapper that export cannot resolve into typed content
	// kinds because the wrapper appears outside every resolved span or the provider
	// parser flagged a content item as unresolved.
	unresolvedInjectedTranscriptError = `conversation %s: cannot filter the pre-compaction transcript in compact summary %s by content kind; export all content kinds with --only all or MCP only: ["all"]`
	// A content filtered export returns this error when a selected live message
	// contains an injected transcript part that does not have usable typed spans or
	// contains a transcript wrapper outside those spans.
	unresolvedLiveTranscriptError = `conversation %s: cannot filter the pre-compaction transcript in live message %d by content kind; export all content kinds with --only all or MCP only: ["all"]`
)

// injectedTranscriptResolver returns typed transcript spans in one summary
// content item. It returns a span only when it establishes the kind of every
// segment in that span.
type injectedTranscriptResolver func(content transcript.CompactedMessageContentItem) []reorienttag.TranscriptSpan

// injectedTranscriptResolvers lists resolvers in the order export tries them.
// Export tries spans that the provider parser attached before framed
// transcripts in the text because a recovered wrapper can quote a framed
// transcript.
func injectedTranscriptResolvers() []injectedTranscriptResolver {
	return []injectedTranscriptResolver{resolveParsedTranscriptSpans, resolveFramedTranscriptSpans}
}

func resolveFramedTranscriptSpans(content transcript.CompactedMessageContentItem) []reorienttag.TranscriptSpan {
	return reorienttag.ParseTranscriptSpans(content.Text)
}

func resolveParsedTranscriptSpans(content transcript.CompactedMessageContentItem) []reorienttag.TranscriptSpan {
	return content.TranscriptSpans
}

// messageTranscriptResolvers returns every resolver for a summary message,
// including the reader that parses framed transcripts from message text. For
// other messages, it returns only resolvers for spans that the provider parser
// attached because user and assistant messages can quote framed transcripts.
func messageTranscriptResolvers(message transcript.CompactedMessageItem) []injectedTranscriptResolver {
	if message.MessageClass == transcript.CompactedMessageClassSummary {
		return injectedTranscriptResolvers()
	}
	return []injectedTranscriptResolver{resolveParsedTranscriptSpans}
}

// resolveInjectedTranscriptSpans returns spans from the first resolver that
// finds spans in the content item.
func resolveInjectedTranscriptSpans(
	content transcript.CompactedMessageContentItem,
	resolvers []injectedTranscriptResolver,
) []reorienttag.TranscriptSpan {
	for _, resolver := range resolvers {
		if spans := resolver(content); len(spans) > 0 {
			return spans
		}
	}
	return nil
}

// unresolvedInjectedTranscriptSummary checks context items in each selected
// segment with a starting summary. A segment matches when its summary message
// has a transcript wrapper outside every resolved span or another message has
// an item that the provider parser flagged as unresolved. It returns the first
// matching segment's summary UUID.
func unresolvedInjectedTranscriptSummary(selection CompactionSegmentSelection) (string, bool) {
	for _, segment := range selection.Segments {
		if !segment.HasStartingSummary {
			continue
		}
		for _, item := range segment.Checkpoint.ContextItems {
			if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil {
				continue
			}
			unresolved := unresolvedParsedTranscript
			if item.Message.MessageClass == transcript.CompactedMessageClassSummary {
				unresolved = unresolvedInjectedTranscript
			}
			if slices.ContainsFunc(item.Message.Content, unresolved) {
				return segment.SummaryUUID, true
			}
		}
	}
	return "", false
}

// unresolvedParsedTranscript reports whether the provider parser flagged the
// content item as containing an unresolved transcript.
func unresolvedParsedTranscript(content transcript.CompactedMessageContentItem) bool {
	return content.UnresolvedTranscript
}

// unresolvedInjectedTranscript reports whether one summary content item's text
// contains a transcript wrapper start after removal of the resolved spans.
func unresolvedInjectedTranscript(content transcript.CompactedMessageContentItem) bool {
	var outside strings.Builder
	previousEnd := 0
	for _, span := range resolveInjectedTranscriptSpans(content, injectedTranscriptResolvers()) {
		outside.WriteString(content.Text[previousEnd:span.Start])
		previousEnd = span.End
	}
	outside.WriteString(content.Text[previousEnd:])
	return reorienttag.ContainsTranscriptWrapper(outside.String())
}

// liveTranscriptPartUnresolved reports whether an injected transcript part
// lacks typed spans or has a transcript wrapper outside those spans.
func liveTranscriptPartUnresolved(part transcript.MessageTextPart) bool {
	if !part.InjectedTranscript {
		return false
	}
	marked := transcript.CompactedMessageContentItem{
		Type:                 injectedChatContentType,
		Text:                 part.Text,
		ImageURL:             "",
		Detail:               "",
		Raw:                  nil,
		TranscriptSpans:      part.TranscriptSpans,
		UnresolvedTranscript: false,
	}
	return len(part.TranscriptSpans) == 0 || unresolvedInjectedTranscript(marked)
}

// unresolvedLiveTranscriptIndex returns the zero-based index of the first
// message with an unresolved injected transcript part and reports whether it
// found one.
func unresolvedLiveTranscriptIndex(messages []transcript.Message) (int, bool) {
	for index, message := range messages {
		if slices.ContainsFunc(message.TextParts, liveTranscriptPartUnresolved) {
			return index, true
		}
	}
	return 0, false
}

// filterLiveTextParts joins nonempty results from selectedLiveText in part
// order with a blank line between results.
func filterLiveTextParts(parts []transcript.MessageTextPart, content ContentKindSet) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		text := selectedLiveText(part, content)
		if text != "" {
			kept = append(kept, text)
		}
	}
	return strings.Join(kept, liveTextPartSeparator)
}

// selectedLiveText returns ordinary text only when the selected content kinds
// include chat. It rebuilds injected transcript frames from segments that
// match the selected content kinds and omits frames without selected segments.
func selectedLiveText(part transcript.MessageTextPart, content ContentKindSet) string {
	if !part.InjectedTranscript {
		if content.Has(ContentKindChat) {
			return part.Text
		}
		return ""
	}
	selected := make([]string, 0, 1)
	for _, span := range part.TranscriptSpans {
		segments := selectedInjectedSegments(span.Segments, content)
		if len(segments) > 0 {
			selected = append(selected, reorienttag.FramedTranscript(segments))
		}
	}
	return strings.Join(selected, liveTextPartSeparator)
}

// expandInjectedTranscriptItems replaces each message that has resolved spans
// with message items for surrounding text and one typed context item per
// segment. It resolves spans for summary messages from both message text and
// parser spans and uses only parser spans for other messages. The content kind
// filter selects the resulting items under the rules for saved Codex context
// items.
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
	if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil {
		return []transcript.CompactedContextItem{item}
	}
	resolvers := messageTranscriptResolvers(*item.Message)
	expanded := make([]transcript.CompactedContextItem, 0)
	prose := make([]transcript.CompactedMessageContentItem, 0)
	resolved := false
	for _, contentItem := range item.Message.Content {
		spans := resolveInjectedTranscriptSpans(contentItem, resolvers)
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
			{Type: injectedChatContentType, Text: text, ImageURL: "", Detail: "", Raw: nil, TranscriptSpans: nil, UnresolvedTranscript: false},
		},
		ContentRaw:   nil,
		MessageClass: transcript.CompactedMessageClassOrdinary,
		Raw:          nil,
	}
	return item
}

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
	spans := resolveInjectedTranscriptSpans(summaryContentItemForText(text, items), injectedTranscriptResolvers())
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
		Type:                 injectedChatContentType,
		Text:                 text,
		ImageURL:             "",
		Detail:               "",
		Raw:                  nil,
		TranscriptSpans:      nil,
		UnresolvedTranscript: false,
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
