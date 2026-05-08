package anthropic

// Constructors for StreamEvent and ContentBlock literals. Each constructor
// names every field on the target struct so the exhaustruct rule is
// satisfied at the call site by routing through these helpers.

// newToolUseStartEvent builds a "tool_use_start" StreamEvent with every
// non-applicable field zeroed.
func newToolUseStartEvent(blockIndex int, toolUseID, toolUseName string) StreamEvent {
	return StreamEvent{
		Kind:        "tool_use_start",
		Text:        "",
		BlockIndex:  blockIndex,
		ToolUseID:   toolUseID,
		ToolUseName: toolUseName,
		PartialJSON: "",
		StopReason:  "",
		Data:        "",
	}
}

// newThinkingStartEvent builds a "thinking" StreamEvent emitted at
// content_block_start with every non-applicable field zeroed.
func newThinkingStartEvent(blockIndex int) StreamEvent {
	return StreamEvent{
		Kind:        "thinking",
		Text:        "",
		BlockIndex:  blockIndex,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
		Data:        "",
	}
}

// newRedactedThinkingStartEvent builds a "redacted_thinking" StreamEvent
// emitted at content_block_start with every non-applicable field zeroed.
func newRedactedThinkingStartEvent(blockIndex int, data string) StreamEvent {
	return StreamEvent{
		Kind:        "redacted_thinking",
		Text:        "",
		BlockIndex:  blockIndex,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
		Data:        data,
	}
}

// newTextDeltaEvent builds a "text" StreamEvent emitted on content_block_delta
// for a text_delta with every non-applicable field zeroed.
func newTextDeltaEvent(blockIndex int, text string) StreamEvent {
	return StreamEvent{
		Kind:        "text",
		Text:        text,
		BlockIndex:  blockIndex,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
		Data:        "",
	}
}

// newToolUseArgDeltaEvent builds a "tool_use_arg_delta" StreamEvent with every
// non-applicable field zeroed.
func newToolUseArgDeltaEvent(blockIndex int, partialJSON string) StreamEvent {
	return StreamEvent{
		Kind:        "tool_use_arg_delta",
		Text:        "",
		BlockIndex:  blockIndex,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: partialJSON,
		StopReason:  "",
		Data:        "",
	}
}

// newThinkingDeltaEvent builds a "thinking" StreamEvent emitted on
// content_block_delta for a thinking_delta with every non-applicable field
// zeroed.
func newThinkingDeltaEvent(blockIndex int, text string) StreamEvent {
	return StreamEvent{
		Kind:        "thinking",
		Text:        text,
		BlockIndex:  blockIndex,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
		Data:        "",
	}
}

// newThinkingSignatureDeltaEvent builds a "thinking_signature" StreamEvent
// with every non-applicable field zeroed.
func newThinkingSignatureDeltaEvent(blockIndex int, signature string) StreamEvent {
	return StreamEvent{
		Kind:        "thinking_signature",
		Text:        signature,
		BlockIndex:  blockIndex,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
		Data:        "",
	}
}

// newToolUseStopEvent builds a "tool_use_stop" StreamEvent with every
// non-applicable field zeroed.
func newToolUseStopEvent(blockIndex int) StreamEvent {
	return StreamEvent{
		Kind:        "tool_use_stop",
		Text:        "",
		BlockIndex:  blockIndex,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  "",
		Data:        "",
	}
}

// newStopEvent builds a terminal "stop" StreamEvent with every non-applicable
// field zeroed.
func newStopEvent(stopReason string) StreamEvent {
	return StreamEvent{
		Kind:        "stop",
		Text:        "",
		BlockIndex:  0,
		ToolUseID:   "",
		ToolUseName: "",
		PartialJSON: "",
		StopReason:  stopReason,
		Data:        "",
	}
}

// newTextContentBlock builds a "text" ContentBlock with every union field
// zeroed.
func newTextContentBlock(text string) ContentBlock {
	return ContentBlock{
		Type:           "text",
		Text:           text,
		ID:             "",
		Name:           "",
		Input:          nil,
		ToolUseID:      "",
		Content:        "",
		CacheReference: "",
		Source:         nil,
		CacheControl:   nil,
		Thinking:       "",
		Signature:      "",
		Data:           "",
	}
}
