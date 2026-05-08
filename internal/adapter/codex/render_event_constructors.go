package codex

import (
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// Constructors for adapterrender.Event literals emitted by the codex
// protocol parser. RedactedData stays empty everywhere because Codex never
// surfaces an Anthropic-style redacted_thinking blob; only the Anthropic
// translator populates it. Each constructor names every field on
// adapterrender.Event so the exhaustruct rule sees a fully-populated literal
// at the call site.

// newAssistantTextDeltaEvent builds an EventAssistantTextDelta carrying a
// text chunk. All non-applicable fields are zeroed.
func newAssistantTextDeltaEvent(text string) adapterrender.Event {
	return adapterrender.Event{
		Kind:             adapterrender.EventAssistantTextDelta,
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

// newReasoningSummaryDeltaEvent builds an EventReasoningDelta for one
// summary chunk on a reasoning item. All non-applicable fields are zeroed.
func newReasoningSummaryDeltaEvent(text, itemID string, summaryIndex *int) adapterrender.Event {
	return adapterrender.Event{
		Kind:             adapterrender.EventReasoningDelta,
		Text:             text,
		ReasoningKind:    "summary",
		SummaryIndex:     summaryIndex,
		ToolCalls:        nil,
		ItemID:           itemID,
		ItemType:         "reasoning",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newReasoningTextDeltaEvent builds an EventReasoningDelta for one
// reasoning_text chunk on a reasoning item. All non-applicable fields are
// zeroed.
func newReasoningTextDeltaEvent(text, itemID string) adapterrender.Event {
	return adapterrender.Event{
		Kind:             adapterrender.EventReasoningDelta,
		Text:             text,
		ReasoningKind:    "text",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           itemID,
		ItemType:         "reasoning",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newStreamReasoningDeltaEvent builds an EventReasoningDelta for streaming
// reasoning summary or text deltas surfaced as they arrive on the wire. The
// caller picks the kind based on the event name.
func newStreamReasoningDeltaEvent(text, kind, itemID string, summaryIndex *int) adapterrender.Event {
	return adapterrender.Event{
		Kind:             adapterrender.EventReasoningDelta,
		Text:             text,
		ReasoningKind:    kind,
		SummaryIndex:     summaryIndex,
		ToolCalls:        nil,
		ItemID:           itemID,
		ItemType:         "reasoning",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newReasoningFinishedEvent builds an EventReasoningFinished optionally
// carrying the per-item encrypted_content blob. itemID, itemType, and
// encryptedContent default to empty when the caller does not have a
// per-item context (e.g. the response.completed terminator). All other
// fields are zeroed.
func newReasoningFinishedEvent(itemID, itemType, encryptedContent string) adapterrender.Event {
	return adapterrender.Event{
		Kind:             adapterrender.EventReasoningFinished,
		Text:             "",
		ReasoningKind:    "",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           itemID,
		ItemType:         itemType,
		EncryptedContent: encryptedContent,
		Signature:        "",
		RedactedData:     "",
	}
}

// newReasoningSignaledEvent builds an EventReasoningSignaled marking the
// first reasoning item observed in the stream. All non-applicable fields
// are zeroed.
func newReasoningSignaledEvent(itemID string) adapterrender.Event {
	return adapterrender.Event{
		Kind:             adapterrender.EventReasoningSignaled,
		Text:             "",
		ReasoningKind:    "",
		SummaryIndex:     nil,
		ToolCalls:        nil,
		ItemID:           itemID,
		ItemType:         "reasoning",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}
}

// newToolCallDeltaEvent builds an EventToolCallDelta carrying the streaming
// tool call shape. All non-applicable fields are zeroed.
func newToolCallDeltaEvent(toolCalls []adapteropenai.ToolCall) adapterrender.Event {
	return adapterrender.Event{
		Kind:             adapterrender.EventToolCallDelta,
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
