package anthropicbackend

import (
	"encoding/json"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// zeroBlock returns a fully zeroed AnthContentBlock literal. Used by
// returns that pair the block with an `ok bool`, where the caller never
// reads the block when ok is false.
func zeroBlock() AnthContentBlock {
	return AnthContentBlock{
		Type:          "",
		Text:          "",
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     "",
		ResultContent: "",
		Source:        nil,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}
}

// newTextBlock builds a "text" content block with all union fields zeroed.
func newTextBlock(text string) AnthContentBlock {
	return AnthContentBlock{
		Type:          "text",
		Text:          text,
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     "",
		ResultContent: "",
		Source:        nil,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}
}

// newImageBlock builds an "image" content block with all union fields zeroed.
func newImageBlock(src *AnthImageSource) AnthContentBlock {
	return AnthContentBlock{
		Type:          "image",
		Text:          "",
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     "",
		ResultContent: "",
		Source:        src,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}
}

// newToolUseBlock builds a "tool_use" content block with all union fields zeroed.
func newToolUseBlock(id, name string, input json.RawMessage) AnthContentBlock {
	return AnthContentBlock{
		Type:          "tool_use",
		Text:          "",
		ID:            id,
		Name:          name,
		Input:         input,
		ToolUseID:     "",
		ResultContent: "",
		Source:        nil,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}
}

// newToolResultBlock builds a "tool_result" content block with all union fields zeroed.
func newToolResultBlock(toolUseID, result string) AnthContentBlock {
	return AnthContentBlock{
		Type:          "tool_result",
		Text:          "",
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     toolUseID,
		ResultContent: result,
		Source:        nil,
		Thinking:      "",
		Signature:     "",
		Data:          "",
	}
}

// newThinkingBlock builds a "thinking" content block with all union fields zeroed.
func newThinkingBlock(body, signature string) AnthContentBlock {
	return AnthContentBlock{
		Type:          "thinking",
		Text:          "",
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     "",
		ResultContent: "",
		Source:        nil,
		Thinking:      body,
		Signature:     signature,
		Data:          "",
	}
}

// newRedactedThinkingBlock builds a "redacted_thinking" content block with all
// union fields zeroed. The opaque server payload rides on the Data field.
func newRedactedThinkingBlock(data string) AnthContentBlock {
	return AnthContentBlock{
		Type:          "redacted_thinking",
		Text:          "",
		ID:            "",
		Name:          "",
		Input:         nil,
		ToolUseID:     "",
		ResultContent: "",
		Source:        nil,
		Thinking:      "",
		Signature:     "",
		Data:          data,
	}
}

// newAssistantTextDeltaEvent builds an EventAssistantTextDelta render.Event
// with every non-applicable field zeroed.
func newAssistantTextDeltaEvent(text string) Event {
	return Event{
		Kind:             EventAssistantTextDelta,
		Text:             text,
		ReasoningKind:    "",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newReasoningSignaledEvent builds an EventReasoningSignaled render.Event with
// every non-applicable field zeroed.
func newReasoningSignaledEvent(reasoningKind string) Event {
	return Event{
		Kind:             EventReasoningSignaled,
		Text:             "",
		ReasoningKind:    reasoningKind,
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newReasoningTextDeltaEvent builds an EventReasoningDelta render.Event for a
// thinking-text delta with every non-applicable field zeroed.
func newReasoningTextDeltaEvent(text string) Event {
	return Event{
		Kind:             EventReasoningDelta,
		Text:             text,
		ReasoningKind:    "text",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newReasoningSignatureDeltaEvent builds an EventReasoningDelta render.Event
// carrying the signature_delta value with every non-applicable field zeroed.
func newReasoningSignatureDeltaEvent(signature string) Event {
	return Event{
		Kind:             EventReasoningDelta,
		Text:             "",
		ReasoningKind:    "text",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        signature,
		RedactedData:     "",
	}
}

// newRedactedThinkingDeltaEvent builds an EventReasoningDelta render.Event for
// a redacted_thinking block with every non-applicable field zeroed.
func newRedactedThinkingDeltaEvent(text, redactedData string) Event {
	return Event{
		Kind:             EventReasoningDelta,
		Text:             text,
		ReasoningKind:    "redacted",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     redactedData,
	}
}

// newReasoningFinishedEvent builds an EventReasoningFinished render.Event with
// every non-applicable field zeroed.
func newReasoningFinishedEvent(reasoningKind string) Event {
	return Event{
		Kind:             EventReasoningFinished,
		Text:             "",
		ReasoningKind:    reasoningKind,
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newToolCallDeltaEvent builds an EventToolCallDelta render.Event with every
// non-applicable field zeroed.
func newToolCallDeltaEvent(toolCalls []adapteropenai.ToolCall) Event {
	return Event{
		Kind:             EventToolCallDelta,
		Text:             "",
		ReasoningKind:    "",
		SummaryIndex:     nil,
		ToolCalls:        toolCalls,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newAssistantRefusalDeltaEvent builds an EventAssistantRefusalDelta
// render.Event with every non-applicable field zeroed.
func newAssistantRefusalDeltaEvent(text string) Event {
	return Event{
		Kind:             adapterrender.EventAssistantRefusalDelta,
		Text:             text,
		ReasoningKind:    "",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}
