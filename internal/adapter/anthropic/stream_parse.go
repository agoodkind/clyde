package anthropic

import (
	"encoding/json"
)

// streamMessageUsage is the usage object inside a message_start event.
type streamMessageUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// streamMessage is the message object inside a message_start event.
type streamMessage struct {
	Usage streamMessageUsage `json:"usage"`
}

// streamMessageStartEvent is the full payload for `event: message_start`.
type streamMessageStartEvent struct {
	Message streamMessage `json:"message"`
}

// streamContentBlockType is the closed enum of Anthropic content block
// types the parser dispatches on at content_block_start. Other values
// fall through to the default arm so a future block type does not crash
// the parser.
type streamContentBlockType string

const (
	streamContentBlockTypeText             streamContentBlockType = "text"
	streamContentBlockTypeToolUse          streamContentBlockType = "tool_use"
	streamContentBlockTypeThinking         streamContentBlockType = "thinking"
	streamContentBlockTypeRedactedThinking streamContentBlockType = "redacted_thinking"
)

// streamContentBlockSpec is the content_block object on content_block_start.
//
// Data carries the opaque base64 blob Anthropic emits inline on a
// `redacted_thinking` content_block_start event. Anthropic does not stream
// a delta for redacted_thinking blocks (the entire payload arrives on the
// start event), so the parser surfaces it directly off this spec without
// waiting for content_block_delta. Empty for every other block type.
type streamContentBlockSpec struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Data string `json:"data,omitempty"`
}

// streamContentBlockStartEvent is the full payload for
// `event: content_block_start`.
type streamContentBlockStartEvent struct {
	Index        int                    `json:"index"`
	ContentBlock streamContentBlockSpec `json:"content_block"`
}

// streamContentBlockStopEvent is the full payload for
// `event: content_block_stop`.
type streamContentBlockStopEvent struct {
	Index int `json:"index"`
}

// streamContentBlockDeltaPayload is the delta object inside a
// content_block_delta event. Signature populates only on the
// `signature_delta` variant Anthropic emits per thinking block.
type streamContentBlockDeltaPayload struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
}

// streamContentBlockDeltaEvent is the full payload for
// `event: content_block_delta`.
type streamContentBlockDeltaEvent struct {
	Index int                            `json:"index"`
	Delta streamContentBlockDeltaPayload `json:"delta"`
}

// streamMessageDeltaPayload is the delta object inside a
// message_delta event (carries stop_reason).
type streamMessageDeltaPayload struct {
	StopReason string `json:"stop_reason"`
}

// streamMessageDeltaUsage is the usage delta on a message_delta event
// (only output_tokens is updated mid-stream). Cache token counts from
// message_start are authoritative; message_delta may echo them for
// completeness.
type streamMessageDeltaUsage struct {
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// streamMessageDeltaEvent is the full payload for `event: message_delta`.
type streamMessageDeltaEvent struct {
	Delta streamMessageDeltaPayload `json:"delta"`
	Usage streamMessageDeltaUsage   `json:"usage"`
}

// streamErrorPayload is the error object inside an error event.
type streamErrorPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// streamErrorEvent is the full payload for `event: error`.
type streamErrorEvent struct {
	Error streamErrorPayload `json:"error"`
}

// dispatchSSE decodes one SSE data payload according to the
// currentEvent name and forwards structured events / usage / stop reasons.
func dispatchSSE(
	eventName, data string,
	sink EventSink,
	usage *Usage,
	stop *string,
	blockTypes map[int]string,
) error {
	switch anthropicSSEEventName(eventName) {
	case anthropicSSEEventPing:
		return nil
	case anthropicSSEEventMessageStart:
		handleSSEMessageStart(data, usage)
		return nil
	case anthropicSSEEventContentBlockStart:
		return handleSSEContentBlockStart(data, sink, blockTypes)
	case anthropicSSEEventContentBlockDelta:
		return handleSSEContentBlockDelta(data, sink)
	case anthropicSSEEventContentBlockStop:
		return handleSSEContentBlockStop(data, sink, blockTypes)
	case anthropicSSEEventMessageDelta:
		handleSSEMessageDelta(data, usage, stop)
		return nil
	case anthropicSSEEventMessageStop:
		return sink(StreamStop{StopReason: *stop})
	case anthropicSSEEventError:
		return handleSSEError(data)
	}
	return nil
}

// anthropicSSEEventName enumerates the SSE event names the Anthropic
// streaming dispatcher recognizes.
type anthropicSSEEventName string

const (
	anthropicSSEEventPing              anthropicSSEEventName = "ping"
	anthropicSSEEventMessageStart      anthropicSSEEventName = "message_start"
	anthropicSSEEventContentBlockStart anthropicSSEEventName = "content_block_start"
	anthropicSSEEventContentBlockDelta anthropicSSEEventName = "content_block_delta"
	anthropicSSEEventContentBlockStop  anthropicSSEEventName = "content_block_stop"
	anthropicSSEEventMessageDelta      anthropicSSEEventName = "message_delta"
	anthropicSSEEventMessageStop       anthropicSSEEventName = "message_stop"
	anthropicSSEEventError             anthropicSSEEventName = "error"
)

// anthropicSSEDeltaType enumerates the delta type strings the
// content_block_delta SSE event carries.
type anthropicSSEDeltaType string

const (
	anthropicSSEDeltaText      anthropicSSEDeltaType = "text_delta"
	anthropicSSEDeltaInputJSON anthropicSSEDeltaType = "input_json_delta"
	anthropicSSEDeltaThinking  anthropicSSEDeltaType = "thinking_delta"
	anthropicSSEDeltaSignature anthropicSSEDeltaType = "signature_delta"
)

func handleSSEMessageStart(data string, usage *Usage) {
	var ev streamMessageStartEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return
	}
	usage.InputTokens = ev.Message.Usage.InputTokens
	usage.OutputTokens = ev.Message.Usage.OutputTokens
	usage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
	usage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
}

func handleSSEContentBlockStart(data string, sink EventSink, blockTypes map[int]string) error {
	var ev streamContentBlockStartEvent
	if err := json.Unmarshal([]byte(data), &ev); err == nil {
		return dispatchContentBlockStart(ev, sink, blockTypes)
	}
	return nil
}

func dispatchContentBlockStart(ev streamContentBlockStartEvent, sink EventSink, blockTypes map[int]string) error {
	t := ev.ContentBlock.Type
	blockTypes[ev.Index] = t
	switch streamContentBlockType(t) {
	case streamContentBlockTypeToolUse:
		return sink(StreamToolUseStart{
			BlockIndex:  ev.Index,
			ToolUseID:   ev.ContentBlock.ID,
			ToolUseName: ev.ContentBlock.Name,
		})
	case streamContentBlockTypeThinking:
		return sink(StreamThinkingStart{BlockIndex: ev.Index})
	case streamContentBlockTypeRedactedThinking:
		// Anthropic emits the opaque payload on the start event itself.
		// There is no redacted_thinking_delta; one event per block
		// carries the data blob and content_block_stop closes it.
		return sink(StreamRedactedThinking{
			BlockIndex: ev.Index,
			Data:       ev.ContentBlock.Data,
		})
	case streamContentBlockTypeText:
		// Plain text content blocks are observed via the per-delta
		// path; no synchronous event is needed at start.
	}
	return nil
}

func handleSSEContentBlockDelta(data string, sink EventSink) error {
	var ev streamContentBlockDeltaEvent
	if err := json.Unmarshal([]byte(data), &ev); err == nil {
		return dispatchContentBlockDelta(ev, sink)
	}
	return nil
}

func dispatchContentBlockDelta(ev streamContentBlockDeltaEvent, sink EventSink) error {
	switch anthropicSSEDeltaType(ev.Delta.Type) {
	case anthropicSSEDeltaText:
		if ev.Delta.Text == "" {
			return nil
		}
		return sink(StreamTextDelta{BlockIndex: ev.Index, Text: ev.Delta.Text})
	case anthropicSSEDeltaInputJSON:
		// Anthropic emits a leading content_block_delta with an empty
		// partial_json to open the tool input stream. Forwarding it
		// produces a tool_call delta with a zero-value Function block
		// that json:"function,omitzero" drops, leaving a bare
		// {"index":N,"type":"function"} chunk on the wire. Cursor's
		// OpenAI SSE parser treats that as a finalize/reset and drops
		// the whole tool call, so the user sees no Read/Glob card.
		if ev.Delta.PartialJSON == "" {
			return nil
		}
		return sink(StreamToolUseArgDelta{
			BlockIndex:  ev.Index,
			PartialJSON: ev.Delta.PartialJSON,
		})
	case anthropicSSEDeltaThinking:
		return sink(StreamThinkingDelta{BlockIndex: ev.Index, Text: ev.Delta.Thinking})
	case anthropicSSEDeltaSignature:
		return sink(StreamThinkingSignature{BlockIndex: ev.Index, Signature: ev.Delta.Signature})
	}
	return nil
}

func handleSSEContentBlockStop(data string, sink EventSink, blockTypes map[int]string) error {
	var ev streamContentBlockStopEvent
	if err := json.Unmarshal([]byte(data), &ev); err == nil {
		if blockTypes[ev.Index] == "tool_use" {
			delete(blockTypes, ev.Index)
			return sink(StreamToolUseStop{BlockIndex: ev.Index})
		}
		delete(blockTypes, ev.Index)
	}
	return nil
}

func handleSSEMessageDelta(data string, usage *Usage, stop *string) {
	var ev streamMessageDeltaEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return
	}
	if ev.Delta.StopReason != "" {
		*stop = ev.Delta.StopReason
	}
	if ev.Usage.OutputTokens > 0 {
		usage.OutputTokens = ev.Usage.OutputTokens
	}
	if ev.Usage.CacheCreationInputTokens > 0 {
		usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
	}
	if ev.Usage.CacheReadInputTokens > 0 {
		usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
	}
}

func handleSSEError(data string) error {
	var ev streamErrorEvent
	if err := json.Unmarshal([]byte(data), &ev); err == nil {
		return newStreamUpstreamError(ErrorKind(ev.Error.Type), ev.Error.Message)
	}
	return newStreamUpstreamError(ErrorKindNone, "anthropic error: "+truncate(data, 400))
}

func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + "..."
}

// newStreamUpstreamError builds the typed UpstreamError for an SSE
// `event: error` frame on a 200 stream. The Anthropic envelope
// `error.type` ("rate_limit_error", "overloaded_error", etc.)
// carries the routing signal because the wire HTTP status was 200
// and Status alone cannot tell the adapter how to classify the
// failure (CLYDE-439).
//
// The synthesized Classification mirrors the routing rule used by
// HTTP-level failures: rate_limit_error and overloaded_error are
// retryable; other types fall through to fatal. The class is the
// source of truth for retry policies and provider notice routing;
// the upstream-to-adapter mapping in
// `anthropic_provider_dispatch.anthropicProviderAdapterError`
// inspects ErrorType to pick the right `upstreamClass*` for the
// OpenAI route family envelope.
func newStreamUpstreamError(errorType ErrorKind, message string) *UpstreamError {
	classification := Classification{
		Class:              ResponseClassFatalError,
		Status:             0,
		TransportError:     nil,
		Retryable:          false,
		HasOverageRejected: false,
		HasOverageActive:   false,
		SurpassedThreshold: false,
		AllowedWarning:     false,
	}
	switch errorType {
	case ErrorKindRateLimit, ErrorKindOverloaded:
		classification.Class = ResponseClassRetryableError
		classification.Retryable = true
	case ErrorKindNone, ErrorKindAuth, ErrorKindInvalidRequest, ErrorKindAPI:
		// Default fatal class is correct for these.
	}
	body := message
	if errorType != ErrorKindNone {
		body = "anthropic error: " + string(errorType) + ": " + message
	}
	return &UpstreamError{
		Classification: classification,
		Status:         0,
		Message:        body,
		Cause:          nil,
		ErrorType:      errorType,
	}
}
