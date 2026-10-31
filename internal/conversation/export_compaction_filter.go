package conversation

import (
	"slices"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

const (
	compactedRoleDeveloper = "developer"
	compactedRoleSystem    = "system"
	compactedDataURLPrefix = "data:"
)

// filterCompactionSelectionItems returns a copy of the segment selection with
// checkpoint context items filtered by content kind. The filter runs before the
// sanitizer, which does not marshal excluded items.
func filterCompactionSelectionItems(
	selection CompactionSegmentSelection,
	content ContentKindSet,
) CompactionSegmentSelection {
	segments := slices.Clone(selection.Segments)
	for i := range segments {
		segments[i].Checkpoint.ContextItems = filterCompactionContextItems(segments[i].Checkpoint.ContextItems, content)
	}
	return CompactionSegmentSelection{
		Selector: selection.Selector,
		Segments: segments,
	}
}

// filterCompactionMessageItems copies each compaction message's metadata and
// filters its context items by content kind. Cached source messages retain their
// original metadata.
func filterCompactionMessageItems(messages []transcript.Message, content ContentKindSet) {
	for i := range messages {
		if messages[i].Compaction == nil {
			continue
		}
		metadata := *messages[i].Compaction
		metadata.ContextItems = filterCompactionContextItems(metadata.ContextItems, content)
		messages[i].Compaction = &metadata
		// A summary message renders its text when system_messages is selected.
		// Unless raw_json_metadata is selected, the text uses the same segment
		// selection as context items.
		if metadata.Kind == transcript.CompactionKindSummary && !content.Has(ContentKindRawJSONMetadata) {
			messages[i].Text = filterInjectedTranscriptText(messages[i].Text, content)
		}
	}
}

// filterCompactionContextItems returns copies of every saved item when the content
// kinds select raw_json_metadata. The export sanitizer later removes encrypted content.
// Otherwise, it returns copies of the items selected by the content kinds, excluding
// raw JSON copies and reasoning encrypted content. It returns nil if the content
// kinds select no items.
func filterCompactionContextItems(
	items []transcript.CompactedContextItem,
	content ContentKindSet,
) []transcript.CompactedContextItem {
	if content.Has(ContentKindRawJSONMetadata) {
		return copyContextItems(items)
	}
	kept := make([]transcript.CompactedContextItem, 0, len(items))
	for _, item := range expandInjectedTranscriptItems(items) {
		if !compactedItemSelected(item, content) {
			continue
		}
		item = compactedItemWithoutRaw(item)
		if !content.Has(ContentKindToolCalls) && !content.Has(ContentKindToolOutputs) {
			item = compactedCallSummary(item)
		}
		kept = append(kept, item)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// compactedItemSelected reports whether the content kinds select a saved
// compaction context item. Chat selects user, assistant, and summary messages.
// System prompts select developer and system messages. Thinking selects reasoning.
// Any tool kind (tools, tool_calls, or tool_outputs) selects tool calls.
// The tool_outputs kind also selects tool outputs. Raw JSON metadata selects
// compaction items and every other item type.
func compactedItemSelected(item transcript.CompactedContextItem, content ContentKindSet) bool {
	switch item.Kind {
	case transcript.CompactedContextItemKindMessage:
		return compactedMessageSelected(item.Message, content)
	case transcript.CompactedContextItemKindReasoning:
		return content.Has(ContentKindThinking)
	case transcript.CompactedContextItemKindLocalShellCall,
		transcript.CompactedContextItemKindFunctionCall,
		transcript.CompactedContextItemKindToolSearchCall,
		transcript.CompactedContextItemKindCustomToolCall,
		transcript.CompactedContextItemKindWebSearchCall,
		transcript.CompactedContextItemKindImageGenerationCall:
		return hasToolContent(content)
	case transcript.CompactedContextItemKindFunctionCallOutput,
		transcript.CompactedContextItemKindCustomToolCallOutput,
		transcript.CompactedContextItemKindToolSearchOutput:
		return content.Has(ContentKindToolOutputs)
	case transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindCompactionTrigger,
		transcript.CompactedContextItemKindContextCompaction,
		transcript.CompactedContextItemKindOther:
		return content.Has(ContentKindRawJSONMetadata)
	default:
		return content.Has(ContentKindRawJSONMetadata)
	}
}

// compactedMessageSelected reports whether content kinds select a message.
// Chat selects summary messages and user or assistant messages. System prompts
// select developer and system messages.
func compactedMessageSelected(message *transcript.CompactedMessageItem, content ContentKindSet) bool {
	if message == nil {
		return false
	}
	if message.MessageClass == transcript.CompactedMessageClassSummary {
		return content.Has(ContentKindChat)
	}
	switch message.Role {
	case compactedRoleDeveloper, compactedRoleSystem:
		return content.Has(ContentKindSystemPrompts)
	default:
		return content.Has(ContentKindChat)
	}
}

// compactedItemWithoutRaw returns a copy of a saved compaction item without raw JSON
// copies, encrypted reasoning content, image data URLs in message content, or image
// generation results. The copy retains message text, reasoning summaries, readable
// reasoning content, tool arguments, and tool outputs. For each removed image data
// URL, the message content item retains its type as a placeholder.
func compactedItemWithoutRaw(item transcript.CompactedContextItem) transcript.CompactedContextItem {
	if item.Message != nil {
		item.Message = compactedMessageWithoutRaw(*item.Message)
	}
	if item.Reasoning != nil {
		item.Reasoning = compactedReasoningWithoutRaw(*item.Reasoning)
	}
	return compactedToolItemWithoutRaw(item)
}

func compactedMessageWithoutRaw(message transcript.CompactedMessageItem) *transcript.CompactedMessageItem {
	message.Raw = nil
	message.ContentRaw = nil
	message.Content = slices.Clone(message.Content)
	for i := range message.Content {
		message.Content[i].Raw = nil
		if strings.HasPrefix(message.Content[i].ImageURL, compactedDataURLPrefix) {
			message.Content[i].ImageURL = ""
		}
	}
	return &message
}

func compactedReasoningWithoutRaw(reasoning transcript.CompactedReasoningItem) *transcript.CompactedReasoningItem {
	reasoning.Raw = nil
	reasoning.SummaryRaw = nil
	reasoning.EncryptedContent = ""
	reasoning.Summary = slices.Clone(reasoning.Summary)
	for i := range reasoning.Summary {
		reasoning.Summary[i].Raw = nil
	}
	return &reasoning
}

// compactedToolItemWithoutRaw returns a copy without raw JSON in tool call and
// tool output payloads.
func compactedToolItemWithoutRaw(item transcript.CompactedContextItem) transcript.CompactedContextItem {
	if item.LocalShellCall != nil {
		call := *item.LocalShellCall
		call.Raw = nil
		item.LocalShellCall = &call
	}
	if item.FunctionCall != nil {
		call := *item.FunctionCall
		call.Raw = nil
		item.FunctionCall = &call
	}
	if item.ToolSearchCall != nil {
		call := *item.ToolSearchCall
		call.Raw = nil
		item.ToolSearchCall = &call
	}
	if item.CustomToolCall != nil {
		call := *item.CustomToolCall
		call.Raw = nil
		item.CustomToolCall = &call
	}
	if item.WebSearchCall != nil {
		call := *item.WebSearchCall
		call.Raw = nil
		item.WebSearchCall = &call
	}
	if item.ImageGenerationCall != nil {
		call := *item.ImageGenerationCall
		call.Raw = nil
		call.Result = ""
		item.ImageGenerationCall = &call
	}
	return compactedToolOutputWithoutRaw(item)
}

// compactedCallSummary reduces a copied saved tool call to the summary detail used by
// the tools selection for ordinary messages. The result contains the tool name, call
// ID, and status. The function clears arguments, input, shell actions, search
// arguments, and image prompts on the copy passed in.
func compactedCallSummary(item transcript.CompactedContextItem) transcript.CompactedContextItem {
	if item.LocalShellCall != nil {
		item.LocalShellCall.ActionRaw = nil
	}
	if item.FunctionCall != nil {
		item.FunctionCall.Arguments = ""
	}
	if item.ToolSearchCall != nil {
		item.ToolSearchCall.ArgumentsRaw = nil
	}
	if item.CustomToolCall != nil {
		item.CustomToolCall.Input = ""
	}
	if item.WebSearchCall != nil {
		item.WebSearchCall.ActionRaw = nil
	}
	if item.ImageGenerationCall != nil {
		item.ImageGenerationCall.RevisedPrompt = ""
	}
	return item
}

func compactedToolOutputWithoutRaw(item transcript.CompactedContextItem) transcript.CompactedContextItem {
	if item.FunctionCallOutput != nil {
		output := *item.FunctionCallOutput
		output.Raw = nil
		item.FunctionCallOutput = &output
	}
	if item.CustomToolCallOutput != nil {
		output := *item.CustomToolCallOutput
		output.Raw = nil
		item.CustomToolCallOutput = &output
	}
	if item.ToolSearchOutput != nil {
		output := *item.ToolSearchOutput
		output.Raw = nil
		item.ToolSearchOutput = &output
	}
	return item
}
