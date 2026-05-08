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
	OpenAIStreamChunk  = adapteropenai.StreamChunk
	OpenAIStreamChoice = adapteropenai.StreamChoice
	OpenAIStreamDelta  = adapteropenai.StreamDelta
	OpenAIUsage        = adapteropenai.Usage
	EventRenderer      = adapterrender.EventRenderer
	Event              = adapterrender.Event
)

const (
	EventAssistantTextDelta = adapterrender.EventAssistantTextDelta
	EventReasoningSignaled  = adapterrender.EventReasoningSignaled
	EventReasoningDelta     = adapterrender.EventReasoningDelta
	EventReasoningFinished  = adapterrender.EventReasoningFinished
	EventToolCallDelta      = adapterrender.EventToolCallDelta
)

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
		renderer:           NewEventRenderer(reqID, modelAlias, "anthropic", slog.Default()),
	}
}

// HandleEventEvents maps one Anthropic stream event to zero or more normalized events.
func (t *StreamTranslator) HandleEventEvents(eventName string, dataJSON []byte) (
	events []Event,
	finished bool,
	finishReason string,
	usage *OpenAIUsage,
	err error,
) {
	evName := resolveStreamEventName(eventName, dataJSON)

	switch evName {
	case "message_start":
		return t.handleMessageStart(dataJSON)
	case "content_block_start":
		return t.handleContentBlockStart(dataJSON)
	case "content_block_delta":
		return t.handleContentBlockDelta(dataJSON)
	case "content_block_stop":
		return t.handleContentBlockStop()
	case "message_delta":
		return t.handleMessageDelta(dataJSON)
	case "message_stop":
		return t.handleMessageStop()
	case "ping":
		return nil, false, "", nil, nil
	case "error":
		return t.handleStreamError(dataJSON)
	default:
		return nil, false, "", nil, nil
	}
}

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
		return nil, false, "", nil, err
	}
	if ev.Message.Usage != nil {
		t.pendingInputTokens = ev.Message.Usage.InputTokens
	}
	return nil, false, "", nil, nil
}

// handleContentBlockStart dispatches to the per-block-type opener.
func (t *StreamTranslator) handleContentBlockStart(dataJSON []byte) ([]Event, bool, string, *OpenAIUsage, error) {
	var ev struct {
		Index        int               `json:"index"`
		ContentBlock *AnthContentBlock `json:"content_block"`
	}
	if err := json.Unmarshal(dataJSON, &ev); err != nil {
		return nil, false, "", nil, err
	}
	if ev.ContentBlock == nil {
		return nil, false, "", nil, nil
	}
	switch ev.ContentBlock.Type {
	case "text":
		t.currentBlockType = "text"
		return nil, false, "", nil, nil
	case "tool_use":
		return t.openToolUseBlock(ev.Index, ev.ContentBlock), false, "", nil, nil
	case "thinking":
		t.currentBlockType = "thinking"
		return []Event{{
			Kind:             EventReasoningSignaled,
			Text:             "",
			ReasoningKind:    "",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           "",
			ItemType:         "",
			EncryptedContent: "",
			Signature:        "",
			RedactedData:     "",
		}}, false, "", nil, nil
	case "redacted_thinking":
		// Anthropic emits the entire opaque payload on the start event
		// (no redacted_thinking_delta exists). Surface a signaled +
		// delta pair so the renderer opens a synthetic envelope and
		// fills it with the user-facing placeholder, plus carries the
		// data blob on the event. The translator emits
		// EventReasoningFinished on content_block_stop below.
		t.currentBlockType = "redacted_thinking"
		return []Event{
			{
				Kind:             EventReasoningSignaled,
				Text:             "",
				ReasoningKind:    "redacted",
				SummaryIndex:     nil,
				ToolCalls:        nil,
				ItemID:           "",
				ItemType:         "",
				EncryptedContent: "",
				Signature:        "",
				RedactedData:     "",
			},
			{
				Kind:             EventReasoningDelta,
				Text:             "[redacted]",
				ReasoningKind:    "redacted",
				SummaryIndex:     nil,
				ToolCalls:        nil,
				ItemID:           "",
				ItemType:         "",
				EncryptedContent: "",
				Signature:        "",
				RedactedData:     ev.ContentBlock.Data,
			},
		}, false, "", nil, nil
	default:
		t.currentBlockType = ev.ContentBlock.Type
		return nil, false, "", nil, nil
	}
}

// openToolUseBlock allocates a tool_call index, records the block mapping,
// and emits the initial tool_call delta carrying the function name.
func (t *StreamTranslator) openToolUseBlock(blockIdx int, block *AnthContentBlock) []Event {
	t.currentBlockType = "tool_use"
	idx := t.toolCallIndex
	t.toolCallIndex++
	t.toolCallByBlockIdx[blockIdx] = idx
	return []Event{{
		Kind:          EventToolCallDelta,
		Text:          "",
		ReasoningKind: "",
		SummaryIndex:  nil,
		ToolCalls: []OpenAIToolCall{{
			Index: idx,
			ID:    block.ID,
			Type:  "function",
			Function: OpenAIToolCallFunction{
				Name:      block.Name,
				Arguments: "",
			},
		}},
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}}
}

// handleContentBlockDelta dispatches the per-delta-type translation.
func (t *StreamTranslator) handleContentBlockDelta(dataJSON []byte) ([]Event, bool, string, *OpenAIUsage, error) {
	var ev struct {
		Index int           `json:"index"`
		Delta *AnthSSEDelta `json:"delta"`
	}
	if err := json.Unmarshal(dataJSON, &ev); err != nil {
		return nil, false, "", nil, err
	}
	if ev.Delta == nil {
		return nil, false, "", nil, nil
	}
	switch ev.Delta.Type {
	case "text_delta":
		t.visibleText.WriteString(ev.Delta.Text)
		return []Event{{
			Kind:             EventAssistantTextDelta,
			Text:             ev.Delta.Text,
			ReasoningKind:    "",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           "",
			ItemType:         "",
			EncryptedContent: "",
			Signature:        "",
			RedactedData:     "",
		}}, false, "", nil, nil
	case "input_json_delta":
		return t.toolArgumentsDelta(ev.Index, ev.Delta.PartialJSON)
	case "thinking_delta":
		return []Event{{
			Kind:             EventReasoningDelta,
			Text:             ev.Delta.Thinking,
			ReasoningKind:    "text",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           "",
			ItemType:         "",
			EncryptedContent: "",
			Signature:        "",
			RedactedData:     "",
		}}, false, "", nil, nil
	case "signature_delta":
		return []Event{{
			Kind:             EventReasoningDelta,
			Text:             "",
			ReasoningKind:    "text",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           "",
			ItemType:         "",
			EncryptedContent: "",
			Signature:        ev.Delta.Signature,
			RedactedData:     "",
		}}, false, "", nil, nil
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
	return []Event{{
		Kind:          EventToolCallDelta,
		Text:          "",
		ReasoningKind: "",
		SummaryIndex:  nil,
		ToolCalls: []OpenAIToolCall{{
			Index: tcIdx,
			Type:  "function",
			Function: OpenAIToolCallFunction{
				Arguments: partialJSON,
			},
		}},
		ItemID:           "",
		ItemType:         "",
		EncryptedContent: "",
		Signature:        "",
		RedactedData:     "",
	}}, false, "", nil, nil
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
		return []Event{{
			Kind:             EventReasoningFinished,
			Text:             "",
			ReasoningKind:    "",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           "",
			ItemType:         "",
			EncryptedContent: "",
			Signature:        "",
			RedactedData:     "",
		}}, false, "", nil, nil
	}
	if t.currentBlockType == "redacted_thinking" {
		t.currentBlockType = ""
		// The data blob was already captured on the delta event in
		// handleContentBlockStart and will be embedded on the close
		// marker as `data-encrypted`. The finished event closes the
		// synthetic envelope.
		return []Event{{
			Kind:             EventReasoningFinished,
			Text:             "",
			ReasoningKind:    "redacted",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           "",
			ItemType:         "",
			EncryptedContent: "",
			Signature:        "",
			RedactedData:     "",
		}}, false, "", nil, nil
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
		return nil, false, "", nil, err
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
		TotalTokens:      t.pendingInputTokens + t.lastOutputTokens,
	}
	var extra []Event
	if t.lastStopReason == "refusal" && t.visibleText.Len() > 0 {
		extra = append(extra, Event{
			Kind:             adapterrender.EventAssistantRefusalDelta,
			Text:             t.visibleText.String(),
			ReasoningKind:    "",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           "",
			ItemType:         "",
			EncryptedContent: "",
			Signature:        "",
			RedactedData:     "",
		})
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
		return nil, false, "", nil, err
	}
	msg := strings.TrimSpace(ev.Error.Message)
	if msg == "" {
		msg = "anthropic stream error"
	}
	return nil, false, "", nil, fmt.Errorf("%s", msg)
}
