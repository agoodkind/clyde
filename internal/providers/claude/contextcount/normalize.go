package contextcount

import (
	"encoding/json"
	"strings"

	generic "goodkind.io/clyde/internal/contextcount"
)

const (
	blockTypeText       = "text"
	blockTypeToolUse    = "tool_use"
	blockTypeToolResult = "tool_result"

	messageRoleAssistant = "assistant"
	messageRoleProgress  = "progress"
	messageRoleSystem    = "system"
	messageRoleUser      = "user"

	localCommandSubtype                = "local_command"
	orphanedToolResultRemovedText      = "[Orphaned tool result removed due to conversation resume]"
	syntheticToolResultPlaceholderText = "[Tool result missing due to internal error]"
)

func normalizeTranscript(transcript generic.Transcript) generic.Transcript {
	out := cloneTranscript(transcript)
	out.Messages = filterMessages(out.Messages)
	out.Messages = mergeConsecutiveSameRole(out.Messages)
	out.Messages = hoistToolResults(out.Messages)
	out.Messages = ensureToolResultPairing(out.Messages)
	out.Messages = ensureNonEmptyMessages(out.Messages)
	return out
}

// ensureNonEmptyMessages guarantees that every message that survives
// the prior stages carries at least one content block. The Anthropic
// messages/count_tokens endpoint rejects an empty content array on any
// message with status 400 and message
// "messages.N: user messages must have non-empty content" or its
// assistant equivalent. This pass walks the array, replaces empty
// content with a placeholder text block, and removes the message
// entirely only when even the placeholder would be invalid (zero-length
// after every prior stripper).
func ensureNonEmptyMessages(messages []generic.Message) []generic.Message {
	out := make([]generic.Message, 0, len(messages))
	for _, message := range messages {
		if len(message.Content) > 0 {
			out = append(out, message)
			continue
		}
		copyMsg := message
		copyMsg.Content = []generic.ContentBlock{newTextBlock("[content removed by compaction]")}
		out = append(out, copyMsg)
	}
	return out
}

func filterMessages(messages []generic.Message) []generic.Message {
	filtered := make([]generic.Message, 0, len(messages))
	for _, message := range messages {
		if shouldDropMessage(message) {
			continue
		}
		filtered = append(filtered, message)
	}
	return filtered
}

func shouldDropMessage(message generic.Message) bool {
	role := strings.TrimSpace(message.Role)
	if role == messageRoleProgress {
		return true
	}
	if role == messageRoleSystem && !isSystemLocalCommandMessage(message) {
		return true
	}
	if isSyntheticAPIErrorMessage(message) {
		return true
	}
	return false
}

func isSystemLocalCommandMessage(message generic.Message) bool {
	if strings.TrimSpace(message.Role) != messageRoleSystem {
		return false
	}
	for _, block := range message.Content {
		if strings.TrimSpace(block.Type) == localCommandSubtype {
			return true
		}
	}
	return false
}

func isSyntheticAPIErrorMessage(message generic.Message) bool {
	if strings.TrimSpace(message.Role) != messageRoleAssistant {
		return false
	}
	for _, block := range message.Content {
		if strings.TrimSpace(block.Type) != blockTypeText {
			continue
		}
		if strings.Contains(block.Text, "isApiErrorMessage") {
			return true
		}
	}
	return false
}

func mergeConsecutiveSameRole(messages []generic.Message) []generic.Message {
	merged := make([]generic.Message, 0, len(messages))
	for _, message := range messages {
		if len(merged) == 0 {
			merged = append(merged, message)
			continue
		}
		last := &merged[len(merged)-1]
		if last.Role != message.Role {
			merged = append(merged, message)
			continue
		}
		last.Content = append(last.Content, message.Content...)
	}
	return merged
}

func hoistToolResults(messages []generic.Message) []generic.Message {
	hoisted := make([]generic.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role != messageRoleUser {
			hoisted = append(hoisted, message)
			continue
		}
		message.Content = hoistToolResultBlocks(message.Content)
		hoisted = append(hoisted, message)
	}
	return hoisted
}

func hoistToolResultBlocks(blocks []generic.ContentBlock) []generic.ContentBlock {
	toolResults := make([]generic.ContentBlock, 0, len(blocks))
	otherBlocks := make([]generic.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == blockTypeToolResult {
			toolResults = append(toolResults, block)
			continue
		}
		otherBlocks = append(otherBlocks, block)
	}
	out := make([]generic.ContentBlock, 0, len(blocks))
	out = append(out, toolResults...)
	out = append(out, otherBlocks...)
	return out
}

func ensureToolResultPairing(messages []generic.Message) []generic.Message {
	result := make([]generic.Message, 0, len(messages))
	allSeenToolUseIDs := map[string]bool{}
	for i := 0; i < len(messages); i++ {
		message := messages[i]
		if message.Role != messageRoleAssistant {
			stripped, changed := stripLeadingOrphanToolResults(message, result)
			if changed {
				if len(stripped.Content) > 0 {
					result = append(result, stripped)
				}
				continue
			}
			result = append(result, message)
			continue
		}

		assistantMessage, toolUseIDs := dedupeAssistantToolUses(message, allSeenToolUseIDs)
		result = append(result, assistantMessage)
		nextMessage, ok := nextUserMessage(messages, i)
		if !ok {
			if len(toolUseIDs) > 0 {
				result = append(result, syntheticToolResultMessage(toolUseIDs))
			}
			continue
		}

		patchedNext := repairNextUserMessage(nextMessage, toolUseIDs)
		i++
		if len(patchedNext.Content) == 0 {
			result = append(result, noContentUserMessage())
			continue
		}
		result = append(result, patchedNext)
	}
	return result
}

func stripLeadingOrphanToolResults(
	message generic.Message,
	result []generic.Message,
) (generic.Message, bool) {
	if message.Role != messageRoleUser {
		return message, false
	}
	if len(result) > 0 && result[len(result)-1].Role == messageRoleAssistant {
		return message, false
	}
	stripped := make([]generic.ContentBlock, 0, len(message.Content))
	changed := false
	for _, block := range message.Content {
		if block.Type == blockTypeToolResult {
			changed = true
			continue
		}
		stripped = append(stripped, block)
	}
	if !changed {
		return message, false
	}
	message.Content = stripped
	if len(message.Content) == 0 && len(result) == 0 {
		message.Content = []generic.ContentBlock{newTextBlock(orphanedToolResultRemovedText)}
	}
	return message, true
}

func dedupeAssistantToolUses(
	message generic.Message,
	allSeenToolUseIDs map[string]bool,
) (generic.Message, []string) {
	content := make([]generic.ContentBlock, 0, len(message.Content))
	toolUseIDs := make([]string, 0, len(message.Content))
	for _, block := range message.Content {
		if block.Type != blockTypeToolUse {
			content = append(content, block)
			continue
		}
		if block.ToolUseID == "" {
			content = append(content, block)
			continue
		}
		if allSeenToolUseIDs[block.ToolUseID] {
			continue
		}
		allSeenToolUseIDs[block.ToolUseID] = true
		toolUseIDs = append(toolUseIDs, block.ToolUseID)
		content = append(content, block)
	}
	if len(content) == 0 {
		content = append(content, newTextBlock("[Tool use interrupted]"))
	}
	message.Content = content
	return message, toolUseIDs
}

func nextUserMessage(messages []generic.Message, assistantIndex int) (generic.Message, bool) {
	nextIndex := assistantIndex + 1
	if nextIndex >= len(messages) {
		return generic.Message{Role: "", Content: nil}, false
	}
	nextMessage := messages[nextIndex]
	if nextMessage.Role != messageRoleUser {
		return generic.Message{Role: "", Content: nil}, false
	}
	return nextMessage, true
}

func repairNextUserMessage(
	message generic.Message,
	toolUseIDs []string,
) generic.Message {
	toolUseSet := map[string]bool{}
	for _, id := range toolUseIDs {
		toolUseSet[id] = true
	}
	existingResultIDs := map[string]bool{}
	for _, block := range message.Content {
		if block.Type != blockTypeToolResult {
			continue
		}
		existingResultIDs[block.ToolUseID] = true
	}

	syntheticBlocks := make([]generic.ContentBlock, 0, len(toolUseIDs))
	for _, id := range toolUseIDs {
		if existingResultIDs[id] {
			continue
		}
		syntheticBlocks = append(syntheticBlocks, syntheticToolResultBlock(id))
	}

	filtered := make([]generic.ContentBlock, 0, len(message.Content)+len(syntheticBlocks))
	seenToolResultIDs := map[string]bool{}
	filtered = append(filtered, syntheticBlocks...)
	for _, block := range message.Content {
		if block.Type != blockTypeToolResult {
			filtered = append(filtered, block)
			continue
		}
		if !toolUseSet[block.ToolUseID] {
			continue
		}
		if seenToolResultIDs[block.ToolUseID] {
			continue
		}
		seenToolResultIDs[block.ToolUseID] = true
		filtered = append(filtered, block)
	}
	message.Content = filtered
	return message
}

func syntheticToolResultMessage(toolUseIDs []string) generic.Message {
	blocks := make([]generic.ContentBlock, 0, len(toolUseIDs))
	for _, id := range toolUseIDs {
		blocks = append(blocks, syntheticToolResultBlock(id))
	}
	return generic.Message{
		Role:    messageRoleUser,
		Content: blocks,
	}
}

func syntheticToolResultBlock(toolUseID string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeToolResult,
		Text:           "",
		Thinking:       "",
		Signature:      "",
		RedactedData:   "",
		ToolUseID:      toolUseID,
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: syntheticToolResultPlaceholderText,
		IsError:        true,
		ImageSource:    emptyImageSource(),
	}
}

func noContentUserMessage() generic.Message {
	return generic.Message{
		Role:    messageRoleUser,
		Content: []generic.ContentBlock{newTextBlock("(no content)")},
	}
}

func newTextBlock(text string) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           blockTypeText,
		Text:           text,
		Thinking:       "",
		Signature:      "",
		RedactedData:   "",
		ToolUseID:      "",
		ToolName:       "",
		ToolInput:      nil,
		ToolResult:     nil,
		ToolResultText: "",
		IsError:        false,
		ImageSource:    emptyImageSource(),
	}
}

func emptyImageSource() generic.ImageSource {
	return generic.ImageSource{
		Type:      "",
		MediaType: "",
		Data:      "",
		URL:       "",
	}
}

func cloneTranscript(transcript generic.Transcript) generic.Transcript {
	system := make([]generic.TextBlock, len(transcript.System))
	copy(system, transcript.System)
	tools := make([]generic.ToolDef, 0, len(transcript.Tools))
	for _, tool := range transcript.Tools {
		tools = append(tools, cloneToolDef(tool))
	}
	messages := make([]generic.Message, 0, len(transcript.Messages))
	for _, message := range transcript.Messages {
		messages = append(messages, cloneMessage(message))
	}
	return generic.Transcript{
		Model:    transcript.Model,
		System:   system,
		Tools:    tools,
		Messages: messages,
	}
}

func cloneToolDef(tool generic.ToolDef) generic.ToolDef {
	return generic.ToolDef{
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: cloneRawMessage(tool.InputSchema),
	}
}

func cloneMessage(message generic.Message) generic.Message {
	return generic.Message{
		Role:    message.Role,
		Content: cloneContentBlocks(message.Content),
	}
}

func cloneContentBlocks(blocks []generic.ContentBlock) []generic.ContentBlock {
	cloned := make([]generic.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		cloned = append(cloned, cloneContentBlock(block))
	}
	return cloned
}

func cloneContentBlock(block generic.ContentBlock) generic.ContentBlock {
	return generic.ContentBlock{
		Type:           block.Type,
		Text:           block.Text,
		Thinking:       block.Thinking,
		Signature:      block.Signature,
		RedactedData:   block.RedactedData,
		ToolUseID:      block.ToolUseID,
		ToolName:       block.ToolName,
		ToolInput:      cloneRawMessage(block.ToolInput),
		ToolResult:     cloneContentBlocks(block.ToolResult),
		ToolResultText: block.ToolResultText,
		IsError:        block.IsError,
		ImageSource:    block.ImageSource,
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	cloned := make([]byte, len(raw))
	copy(cloned, raw)
	return json.RawMessage(cloned)
}
