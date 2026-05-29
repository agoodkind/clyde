package anthropicbackend

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/adapter/finishreason"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

type (
	// OpenAIStreamChunk is part of Clyde's typed adapter surface.
	OpenAIStreamChunk = adapteropenai.StreamChunk
	// OpenAIStreamChoice is part of Clyde's typed adapter surface.
	OpenAIStreamChoice = adapteropenai.StreamChoice
	// OpenAIStreamDelta is part of Clyde's typed adapter surface.
	OpenAIStreamDelta = adapteropenai.StreamDelta
	// OpenAIUsage is part of Clyde's typed adapter surface.
	OpenAIUsage = adapteropenai.Usage
	// EventRenderer is part of Clyde's typed adapter surface.
	EventRenderer = adapterrender.EventRenderer
	// Event is part of Clyde's typed adapter surface.
	Event = adapterrender.Event
)

const (
	// EventAssistantTextDelta aliases the render package constant for use within
	// the anthropic backend package.
	EventAssistantTextDelta = adapterrender.EventAssistantTextDelta
	// EventAssistantRefusalDelta aliases the render package constant for use within
	// the anthropic backend package.
	EventAssistantRefusalDelta = adapterrender.EventAssistantRefusalDelta
	// EventReasoningSignaled aliases the render package constant for use within
	// the anthropic backend package.
	EventReasoningSignaled = adapterrender.EventReasoningSignaled
	// EventReasoningDelta aliases the render package constant for use within
	// the anthropic backend package.
	EventReasoningDelta = adapterrender.EventReasoningDelta
	// EventReasoningFinished aliases the render package constant for use within
	// the anthropic backend package.
	EventReasoningFinished = adapterrender.EventReasoningFinished
	// EventToolCallDelta aliases the render package constant for use within
	// the anthropic backend package.
	EventToolCallDelta = adapterrender.EventToolCallDelta
)

// NewEventRenderer is part of Clyde's typed adapter surface.
var NewEventRenderer = adapterrender.NewEventRenderer

// StreamTranslator converts Anthropic SSE events into normalized render events.
type StreamTranslator struct {
	currentBlockType   string
	toolCallIndex      int
	toolCallByBlockIdx map[int]int
	pendingInputTokens int
	lastStopReason     string
	lastOutputTokens   int
	visibleText        strings.Builder
	renderer           *EventRenderer
}

// NewStreamTranslator builds per-request stream state.
func NewStreamTranslator(reqID, modelAlias string) *StreamTranslator {
	return &StreamTranslator{
		toolCallByBlockIdx: make(map[int]int),
		renderer:           NewEventRenderer(reqID, modelAlias, "anthropic", slog.Default()), currentBlockType:

		// HandleEventEvents maps one Anthropic stream event to zero or more normalized events.
		"", toolCallIndex: 0, pendingInputTokens: 0, lastStopReason: "", lastOutputTokens: 0, visibleText: strings.
					Builder{},
	}
}

// HandleEventEvents is part of Clyde's typed adapter surface.
func (t *StreamTranslator) HandleEventEvents(eventName string, dataJSON []byte) (
	events []Event,
	finished bool,
	finishReason string,
	usage *OpenAIUsage,
	err error,
) {
	evName := resolveStreamEventName(eventName, dataJSON)

	switch anthropicBackendSSEEvent(evName) {
	case anthropicBackendSSEMessageStart:
		return t.handleMessageStart(dataJSON)
	case anthropicBackendSSEContentBlockStart:
		return t.handleContentBlockStart(dataJSON)
	case anthropicBackendSSEContentBlockDelta:
		return t.handleContentBlockDelta(dataJSON)
	case anthropicBackendSSEContentBlockStop:
		return t.handleContentBlockStop()
	case anthropicBackendSSEMessageDelta:
		return t.handleMessageDelta(dataJSON)
	case anthropicBackendSSEMessageStop:
		return t.handleMessageStop()
	case anthropicBackendSSEPing:
		return nil, false, "", nil, nil
	case anthropicBackendSSEError:
		return t.handleStreamError(dataJSON)
	default:
		return nil, false, "", nil, nil
	}
}

// anthropicBackendSSEEvent enumerates the SSE event names the
// translator dispatcher recognizes.
type anthropicBackendSSEEvent string

const (
	anthropicBackendSSEMessageStart      anthropicBackendSSEEvent = "message_start"
	anthropicBackendSSEContentBlockStart anthropicBackendSSEEvent = "content_block_start"
	anthropicBackendSSEContentBlockDelta anthropicBackendSSEEvent = "content_block_delta"
	anthropicBackendSSEContentBlockStop  anthropicBackendSSEEvent = "content_block_stop"
	anthropicBackendSSEMessageDelta      anthropicBackendSSEEvent = "message_delta"
	anthropicBackendSSEMessageStop       anthropicBackendSSEEvent = "message_stop"
	anthropicBackendSSEPing              anthropicBackendSSEEvent = "ping"
	anthropicBackendSSEError             anthropicBackendSSEEvent = "error"
)

// anthropicBackendSSEDelta enumerates the delta type strings the
// translator dispatches on inside content_block_delta events.
type anthropicBackendSSEDelta string

const (
	anthropicBackendSSEDeltaText      anthropicBackendSSEDelta = "text_delta"
	anthropicBackendSSEDeltaInputJSON anthropicBackendSSEDelta = "input_json_delta"
	anthropicBackendSSEDeltaThinking  anthropicBackendSSEDelta = "thinking_delta"
	anthropicBackendSSEDeltaSignature anthropicBackendSSEDelta = "signature_delta"
)

// resolveStreamEventName trims the SSE event name and falls back to the
// embedded `type` field when the line lacked an explicit event header.
func resolveStreamEventName(eventName string, dataJSON []byte) string {
	evName := strings.TrimSpace(eventName)
	if evName != "" {
		return evName
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(dataJSON, &probe); err == nil && probe.Type != "" {
		return probe.Type
	}
	return evName
}

// handleMessageStart records the upstream's reported input-token count.
func (t *StreamTranslator) handleMessageStart(dataJSON []byte) ([]Event, bool, string, *OpenAIUsage, error) {
	var ev struct {
		Message struct {
			Usage *AnthSSEUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(dataJSON, &ev); err != nil {
		slog.Warn("adapter.anthropic.stream.message_start_unmarshal_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return nil, false, "", nil, fmt.Errorf("unmarshal anthropic message_start event: %w", err)
	}
	if ev.Message.Usage != nil {
		t.pendingInputTokens = ev.Message.Usage.InputTokens
	}
	return nil, false, "", nil, nil
}

// handleContentBlockStart dispatches to the per-block-type opener.
func (t *StreamTranslator) handleContentBlockStart(dataJSON []byte) ([]Event, bool, string, *OpenAIUsage, error) {
	var ev struct {
		Index        int             `json:"index"`
		ContentBlock json.RawMessage `json:"content_block"`
	}
	if err := json.Unmarshal(dataJSON, &ev); err != nil {
		slog.Warn("adapter.anthropic.stream.content_block_start_unmarshal_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return nil, false, "", nil, fmt.Errorf("unmarshal anthropic content_block_start event: %w", err)
	}
	if len(ev.ContentBlock) == 0 {
		return nil, false, "", nil, nil
	}
	block, ok, err := UnmarshalContentBlock(ev.ContentBlock)
	if err != nil {
		return nil, false, "", nil, err
	}
	if !ok {
		return nil, false, "", nil, nil
	}
	switch b := block.(type) {
	case TextBlock:
		t.currentBlockType = "text"
		return nil, false, "", nil, nil
	case ToolUseBlock:
		return t.openToolUseBlock(ev.Index, b), false, "", nil, nil
	case ThinkingBlock:
		t.currentBlockType = "thinking"
		return []Event{reasoningSignaledEvent("")}, false, "", nil, nil
	case RedactedThinkingBlock:
		// Anthropic emits the entire opaque payload on the start event
		// (no redacted_thinking_delta exists). Surface a signaled +
		// delta pair so the renderer opens a synthetic envelope and
		// fills it with the user-facing placeholder, plus carries the
		// data blob on the event. The translator emits
		// EventReasoningFinished on content_block_stop below.
		t.currentBlockType = "redacted_thinking"
		return []Event{
			reasoningSignaledEvent("redacted"),
			redactedThinkingDeltaEvent("[redacted]", b.Data),
		}, false, "", nil, nil
	default:
		t.currentBlockType = block.blockType()
		return nil, false, "", nil, nil
	}
}

// openToolUseBlock allocates a tool_call index, records the block mapping,
// and emits the initial tool_call delta carrying the function name.
func (t *StreamTranslator) openToolUseBlock(blockIdx int, block ToolUseBlock) []Event {
	t.currentBlockType = "tool_use"
	idx := t.toolCallIndex
	t.toolCallIndex++
	t.toolCallByBlockIdx[blockIdx] = idx
	return []Event{toolCallDeltaEvent([]OpenAIToolCall{{
		Index: idx,
		ID:    block.ID,
		Type:  "function",
		Function: OpenAIToolCallFunction{
			Name:      block.Name,
			Arguments: "",
		},
	}})}
}

// handleContentBlockDelta dispatches the per-delta-type translation.
func (t *StreamTranslator) handleContentBlockDelta(dataJSON []byte) ([]Event, bool, string, *OpenAIUsage, error) {
	var ev struct {
		Index int           `json:"index"`
		Delta *AnthSSEDelta `json:"delta"`
	}
	if err := json.Unmarshal(dataJSON, &ev); err != nil {
		slog.Warn("adapter.anthropic.stream.content_block_delta_unmarshal_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return nil, false, "", nil, fmt.Errorf("unmarshal anthropic content_block_delta event: %w", err)
	}
	if ev.Delta == nil {
		return nil, false, "", nil, nil
	}
	switch anthropicBackendSSEDelta(ev.Delta.Type) {
	case anthropicBackendSSEDeltaText:
		t.visibleText.WriteString(ev.Delta.Text)
		return []Event{assistantTextDeltaEvent(ev.Delta.Text)}, false, "", nil, nil
	case anthropicBackendSSEDeltaInputJSON:
		return t.toolArgumentsDelta(ev.Index, ev.Delta.PartialJSON)
	case anthropicBackendSSEDeltaThinking:
		return []Event{reasoningTextDeltaEvent(ev.Delta.Thinking)}, false, "", nil, nil
	case anthropicBackendSSEDeltaSignature:
		return []Event{reasoningSignatureDeltaEvent(ev.Delta.Signature)}, false, "", nil, nil
	default:
		return nil, false, "", nil, nil
	}
}

// toolArgumentsDelta emits a tool_call argument fragment, dropping empty
// partial_json payloads that Cursor's SSE parser would treat as a finalize.
func (t *StreamTranslator) toolArgumentsDelta(blockIdx int, partialJSON string) ([]Event, bool, string, *OpenAIUsage, error) {
	tcIdx, ok := t.toolCallByBlockIdx[blockIdx]
	if !ok {
		return nil, false, "", nil, fmt.Errorf("unknown tool block index %d", blockIdx)
	}
	// Empty partial_json carries no information. Forwarding it
	// produces a tool_call delta whose Function field is the
	// zero value, which json:"function,omitzero" drops, leaving
	// a bare {"index":N,"type":"function"} chunk on the wire.
	// Cursor's OpenAI SSE parser treats that as a finalize and
	// drops the entire tool call. Defense in depth alongside
	// the same guard in stream_parse.go::dispatchSSE.
	if partialJSON == "" {
		return nil, false, "", nil, nil
	}
	return []Event{toolCallDeltaEvent([]OpenAIToolCall{{
		Index: tcIdx,
		Type:  "function",
		Function: OpenAIToolCallFunction{
			Arguments: partialJSON, Name: "",
		}, ID: "",
	}})}, false, "", nil, nil
}

// handleContentBlockStop closes the active block and, for thinking blocks,
// emits the reasoning-finished marker the renderer relies on.
func (t *StreamTranslator) handleContentBlockStop() ([]Event, bool, string, *OpenAIUsage, error) {
	// When an Anthropic thinking block ends, emit a reasoning-finished
	// event so the renderer closes the synthetic envelope. The
	// adapter mapper calls adapterrender.ExtractSyntheticParts on
	// the return trip and materializes each kind per provider config:
	// Anthropic emits a native thinking content block; Codex drops by
	// default. The cached prefix stays byte-stable across turns.
	if t.currentBlockType == "thinking" {
		t.currentBlockType = ""
		return []Event{reasoningFinishedEvent("")}, false, "", nil, nil
	}
	if t.currentBlockType == "redacted_thinking" {
		t.currentBlockType = ""
		// The data blob was already captured on the delta event in
		// handleContentBlockStart and will be embedded on the close
		// marker as `data-encrypted`. The finished event closes the
		// synthetic envelope.
		return []Event{reasoningFinishedEvent("redacted")}, false, "", nil, nil
	}
	t.currentBlockType = ""
	return nil, false, "", nil, nil
}

// handleMessageDelta records the upstream's stop reason and output-token count.
func (t *StreamTranslator) handleMessageDelta(dataJSON []byte) ([]Event, bool, string, *OpenAIUsage, error) {
	var ev struct {
		Delta struct {
			StopReason   string `json:"stop_reason"`
			StopSequence string `json:"stop_sequence"`
		} `json:"delta"`
		Usage *AnthSSEUsage `json:"usage"`
	}
	if err := json.Unmarshal(dataJSON, &ev); err != nil {
		slog.Warn("adapter.anthropic.stream.message_delta_unmarshal_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return nil, false, "", nil, fmt.Errorf("unmarshal anthropic message_delta event: %w", err)
	}
	if ev.Delta.StopReason != "" {
		t.lastStopReason = ev.Delta.StopReason
	}
	if ev.Usage != nil {
		t.lastOutputTokens = ev.Usage.OutputTokens
	}
	return nil, false, "", nil, nil
}

// handleMessageStop returns the terminal usage tally and any refusal text.
func (t *StreamTranslator) handleMessageStop() ([]Event, bool, string, *OpenAIUsage, error) {
	reason := finishreason.FromAnthropicStream(t.lastStopReason)
	u := &OpenAIUsage{
		PromptTokens:     t.pendingInputTokens,
		CompletionTokens: t.lastOutputTokens,
		TotalTokens:      t.pendingInputTokens + t.lastOutputTokens, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0,
	}
	var extra []Event
	if t.lastStopReason == "refusal" && t.visibleText.Len() > 0 {
		extra = append(extra, assistantRefusalDeltaEvent(t.visibleText.String()))
	}
	return extra, true, reason, u, nil
}

// handleStreamError surfaces an upstream error event as a Go error.
func (t *StreamTranslator) handleStreamError(dataJSON []byte) ([]Event, bool, string, *OpenAIUsage, error) {
	var ev struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(dataJSON, &ev); err != nil {
		slog.Warn("adapter.anthropic.stream.error_event_unmarshal_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return nil, false, "", nil, fmt.Errorf("unmarshal anthropic error event: %w", err)
	}
	msg := strings.TrimSpace(ev.Error.Message)
	if msg == "" {
		msg = "anthropic stream error"
	}
	return nil, false, "", nil, fmt.Errorf("%s", msg)
}

// assistantTextDeltaEvent builds a TextDelta render.Event.
func assistantTextDeltaEvent(text string) Event {
	return adapterrender.TextDelta{Text: text}
}

// assistantRefusalDeltaEvent builds a RefusalDelta render.Event.
func assistantRefusalDeltaEvent(text string) Event {
	return adapterrender.RefusalDelta{Text: text}
}

// reasoningSignaledEvent builds a ReasoningSignaled render.Event.
func reasoningSignaledEvent(reasoningKind string) Event {
	return adapterrender.ReasoningSignaled{
		ReasoningKind: reasoningKind,
		ItemID:        "",
		ItemType:      "",
	}
}

// reasoningTextDeltaEvent builds a ReasoningDelta for a thinking-text delta.
func reasoningTextDeltaEvent(text string) Event {
	return adapterrender.ReasoningDelta{
		Text:          text,
		ReasoningKind: "text",
		SummaryIndex:  nil,
		Signature:     "",
		RedactedData:  "",
		ItemID:        "",
		ItemType:      "",
	}
}

// reasoningSignatureDeltaEvent builds a ReasoningDelta carrying the
// signature_delta value.
func reasoningSignatureDeltaEvent(signature string) Event {
	return adapterrender.ReasoningDelta{
		Text:          "",
		ReasoningKind: "text",
		SummaryIndex:  nil,
		Signature:     signature,
		RedactedData:  "",
		ItemID:        "",
		ItemType:      "",
	}
}

// redactedThinkingDeltaEvent builds a ReasoningDelta for a redacted_thinking block.
func redactedThinkingDeltaEvent(text, redactedData string) Event {
	return adapterrender.ReasoningDelta{
		Text:          text,
		ReasoningKind: "redacted",
		SummaryIndex:  nil,
		Signature:     "",
		RedactedData:  redactedData,
		ItemID:        "",
		ItemType:      "",
	}
}

// reasoningFinishedEvent builds a ReasoningFinished render.Event.
func reasoningFinishedEvent(reasoningKind string) Event {
	return adapterrender.ReasoningFinished{
		ReasoningKind:    reasoningKind,
		EncryptedContent: "",
		Signature:        "",
		ItemID:           "",
		ItemType:         "",
	}
}

// toolCallDeltaEvent builds a ToolCallDelta render.Event.
func toolCallDeltaEvent(toolCalls []adapteropenai.ToolCall) Event {
	return adapterrender.ToolCallDelta{ToolCalls: toolCalls}
}
