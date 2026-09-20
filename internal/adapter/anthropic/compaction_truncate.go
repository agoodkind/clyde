package anthropic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"goodkind.io/clyde/internal/adapter/content"
)

// interruptedToolResultText is the content of a synthetic tool result. Claude
// Code emits the same shape for a tool call with no result (claude-code source,
// src/utils/messages.ts:5318-5325).
const interruptedToolResultText = "[Tool use was interrupted]"

// truncateError logs one truncation failure and returns it named. Every failure
// in this file is a malformed request body, and the caller forwards the
// original request unmodified, so this log is the only record of the reason.
func truncateError(operation string, err error) error {
	slog.Warn("adapter.anthropic.compaction_truncate_failed",
		"concern", string(anthropicRequestLog),
		"component", "adapter",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}

// CompactionCut is where the split divides the boundary message. MessageIndex
// and SegmentIndex address the boundary segment. HeadRunes is how much of that
// segment's text the model summarizes.
type CompactionCut struct {
	MessageIndex int
	SegmentIndex int
	HeadRunes    int
}

// TruncateCompactionRequest rewrites body to end the conversation at the cut.
// It keeps every message before the boundary message, the boundary message
// truncated at the cut, and every message from instructionStart onward. Every
// top-level field other than messages keeps its bytes.
//
// Anthropic requires a result for every tool call in the same request.
// Truncation deletes the results that answered the retained calls, so this
// appends one synthetic result per unanswered call.
func TruncateCompactionRequest(
	body []byte,
	cut CompactionCut,
	instructionStart int,
) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, truncateError("decode compaction request body", err)
	}
	rawMessages, ok := top["messages"]
	if !ok {
		return nil, fmt.Errorf("compaction request body has no messages field")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return nil, truncateError("decode compaction request messages", err)
	}
	if cut.MessageIndex < 0 || cut.MessageIndex >= len(messages) {
		return nil, fmt.Errorf("cut message index %d out of range %d", cut.MessageIndex, len(messages))
	}
	if instructionStart < cut.MessageIndex || instructionStart > len(messages) {
		return nil, fmt.Errorf("instruction start %d out of range %d", instructionStart, len(messages))
	}

	kept := make([]json.RawMessage, 0, cut.MessageIndex+len(messages)-instructionStart+2)
	kept = append(kept, messages[:cut.MessageIndex]...)

	boundary, err := truncateMessage(messages[cut.MessageIndex], cut.SegmentIndex, cut.HeadRunes)
	if err != nil {
		return nil, err
	}
	if boundary != nil {
		kept = append(kept, boundary)
	}

	synthetic, err := syntheticResultMessage(kept)
	if err != nil {
		return nil, err
	}
	if synthetic != nil {
		kept = append(kept, synthetic)
	}

	kept = append(kept, messages[instructionStart:]...)

	encodedMessages, err := json.Marshal(kept)
	if err != nil {
		return nil, truncateError("encode truncated messages", err)
	}
	top["messages"] = encodedMessages
	out, err := json.Marshal(top)
	if err != nil {
		return nil, truncateError("encode truncated compaction request", err)
	}
	return out, nil
}

// truncateMessage keeps the content blocks before segmentIndex and the first
// headRunes of the block at segmentIndex. It returns nil when no block remains.
func truncateMessage(raw json.RawMessage, segmentIndex, headRunes int) (json.RawMessage, error) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, truncateError("decode boundary message", err)
	}
	rawContent, ok := message["content"]
	if !ok {
		return raw, nil
	}
	trimmed := strings.TrimSpace(string(rawContent))
	if trimmed != "" && trimmed[0] == '"' {
		var plain string
		if err := json.Unmarshal(rawContent, &plain); err != nil {
			return nil, truncateError("decode boundary message text", err)
		}
		head := headRunesOf(plain, headRunes)
		if head == "" {
			return nil, nil
		}
		encoded, marshalErr := json.Marshal(head)
		if marshalErr != nil {
			return nil, truncateError("encode boundary message text", marshalErr)
		}
		message["content"] = encoded
		return marshalMessage(message)
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(rawContent, &blocks); err != nil {
		return nil, truncateError("decode boundary message blocks", err)
	}
	if segmentIndex < 0 || segmentIndex > len(blocks) {
		return nil, fmt.Errorf("cut segment index %d out of range %d", segmentIndex, len(blocks))
	}
	kept := make([]json.RawMessage, 0, segmentIndex+1)
	kept = append(kept, blocks[:segmentIndex]...)
	if segmentIndex < len(blocks) && headRunes > 0 {
		truncated, err := truncateBlock(blocks[segmentIndex], headRunes)
		if err != nil {
			return nil, err
		}
		if truncated != nil {
			kept = append(kept, truncated)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	encoded, marshalErr := json.Marshal(kept)
	if marshalErr != nil {
		return nil, truncateError("encode boundary message blocks", marshalErr)
	}
	message["content"] = encoded
	return marshalMessage(message)
}

func marshalMessage(message map[string]json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, truncateError("encode boundary message", err)
	}
	return encoded, nil
}

// truncateBlock keeps the first headRunes of one content block's text. A
// thinking block keeps its signature, which Anthropic validates against the
// block. A tool call keeps its input a JSON object, which Anthropic requires.
func truncateBlock(raw json.RawMessage, headRunes int) (json.RawMessage, error) {
	var block map[string]json.RawMessage
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, truncateError("decode boundary block", err)
	}
	var blockType string
	if rawType, ok := block["type"]; ok {
		if err := json.Unmarshal(rawType, &blockType); err != nil {
			return nil, truncateError("decode boundary block type", err)
		}
	}
	switch wireBlockType(blockType) {
	case wireBlockText:
		return truncateStringField(block, "text", headRunes)
	case wireBlockThinking:
		return truncateStringField(block, "thinking", headRunes)
	case wireBlockToolResult:
		return truncateToolResultBlock(block, headRunes)
	case wireBlockToolUse:
		return truncateToolUseBlock(block, headRunes)
	case wireBlockImage, wireBlockRedactedThinking:
		return raw, nil
	}
	return raw, nil
}

func truncateStringField(
	block map[string]json.RawMessage,
	field string,
	headRunes int,
) (json.RawMessage, error) {
	rawValue, ok := block[field]
	if !ok {
		return marshalBlock(block)
	}
	var value string
	if err := json.Unmarshal(rawValue, &value); err != nil {
		return nil, truncateError("decode boundary block "+field, err)
	}
	encoded, err := json.Marshal(headRunesOf(value, headRunes))
	if err != nil {
		return nil, truncateError("encode boundary block "+field, err)
	}
	block[field] = encoded
	return marshalBlock(block)
}

// truncateToolResultBlock keeps the first headRunes of the result. A result
// whose content is an array of blocks becomes the truncated flattened text,
// which Anthropic accepts in the string form.
func truncateToolResultBlock(
	block map[string]json.RawMessage,
	headRunes int,
) (json.RawMessage, error) {
	rawContent, ok := block["content"]
	if !ok {
		return marshalBlock(block)
	}
	head := headRunesOf(flattenToolResult(rawContent), headRunes)
	encoded, err := json.Marshal(head)
	if err != nil {
		return nil, truncateError("encode boundary tool result", err)
	}
	block["content"] = encoded
	return marshalBlock(block)
}

// truncateToolUseBlock keeps whole input keys until headRunes runs out, then
// truncates the last string value it reaches. Anthropic rejects an input that
// is not a JSON object, so the result stays an object even when it is empty.
func truncateToolUseBlock(
	block map[string]json.RawMessage,
	headRunes int,
) (json.RawMessage, error) {
	rawInput, ok := block["input"]
	if !ok {
		return marshalBlock(block)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return marshalBlock(block)
	}
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	truncatedInput := map[string]json.RawMessage{}
	budget := headRunes
	for _, key := range keys {
		value := input[key]
		size := len([]rune(string(value)))
		if size <= budget {
			truncatedInput[key] = value
			budget -= size
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			encoded, marshalErr := json.Marshal(headRunesOf(text, budget))
			if marshalErr != nil {
				return nil, truncateError("encode boundary tool input", marshalErr)
			}
			truncatedInput[key] = encoded
		}
		break
	}
	encoded, err := json.Marshal(truncatedInput)
	if err != nil {
		return nil, truncateError("encode boundary tool input object", err)
	}
	block["input"] = encoded
	return marshalBlock(block)
}

func marshalBlock(block map[string]json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(block)
	if err != nil {
		return nil, truncateError("encode boundary block", err)
	}
	return encoded, nil
}

func headRunesOf(s string, count int) string {
	if count <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= count {
		return s
	}
	return string(runes[:count])
}

// syntheticResultMessage returns one user message answering every tool call in
// kept that no result answers. It returns nil when every call has its result.
func syntheticResultMessage(kept []json.RawMessage) (json.RawMessage, error) {
	calls := make([]string, 0)
	answered := map[string]bool{}
	for _, raw := range kept {
		var message compactionWireMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, truncateError("decode kept message", err)
		}
		parts, _ := NormalizeContent(message.Content)
		for _, part := range parts {
			switch part.Kind {
			case content.PartToolUse:
				if part.ID != "" {
					calls = append(calls, part.ID)
				}
			case content.PartToolResult:
				if part.ToolUseID != "" {
					answered[part.ToolUseID] = true
				}
			case content.PartText,
				content.PartImage,
				content.PartAudio,
				content.PartRefusal,
				content.PartThinking,
				content.PartUnsupported:
			}
		}
	}
	blocks := make([]syntheticToolResult, 0, len(calls))
	for _, id := range calls {
		if answered[id] {
			continue
		}
		blocks = append(blocks, syntheticToolResult{
			Type:      "tool_result",
			ToolUseID: id,
			Content:   interruptedToolResultText,
			IsError:   true,
		})
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(syntheticResultWireMessage{Role: "user", Content: blocks})
	if err != nil {
		return nil, truncateError("encode synthetic tool result message", err)
	}
	return encoded, nil
}

type syntheticToolResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

type syntheticResultWireMessage struct {
	Role    string                `json:"role"`
	Content []syntheticToolResult `json:"content"`
}
