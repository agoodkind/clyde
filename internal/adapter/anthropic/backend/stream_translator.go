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
	evName := strings.TrimSpace(eventName)
	if evName == "" {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(dataJSON, &probe); err == nil && probe.Type != "" {
			evName = probe.Type
		}
	}

	switch evName {
	case "message_start":
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

	case "content_block_start":
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
		case "tool_use":
			t.currentBlockType = "tool_use"
			idx := t.toolCallIndex
			t.toolCallIndex++
			t.toolCallByBlockIdx[ev.Index] = idx
			return []Event{{
				Kind: EventToolCallDelta,
				ToolCalls: []OpenAIToolCall{{
					Index: idx,
					ID:    ev.ContentBlock.ID,
					Type:  "function",
					Function: OpenAIToolCallFunction{
						Name:      ev.ContentBlock.Name,
						Arguments: "",
					},
				}},
			}}, false, "", nil, nil
		case "thinking":
			t.currentBlockType = "thinking"
			return []Event{{Kind: EventReasoningSignaled}}, false, "", nil, nil
		default:
			t.currentBlockType = ev.ContentBlock.Type
		}
		return nil, false, "", nil, nil

	case "content_block_delta":
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
			return []Event{{Kind: EventAssistantTextDelta, Text: ev.Delta.Text}}, false, "", nil, nil
		case "input_json_delta":
			tcIdx, ok := t.toolCallByBlockIdx[ev.Index]
			if !ok {
				return nil, false, "", nil, fmt.Errorf("unknown tool block index %d", ev.Index)
			}
			// Empty partial_json carries no information. Forwarding it
			// produces a tool_call delta whose Function field is the
			// zero value, which json:"function,omitzero" drops, leaving
			// a bare {"index":N,"type":"function"} chunk on the wire.
			// Cursor's OpenAI SSE parser treats that as a finalize and
			// drops the entire tool call. Defense in depth alongside
			// the same guard in stream_parse.go::dispatchSSE.
			if ev.Delta.PartialJSON == "" {
				return nil, false, "", nil, nil
			}
			return []Event{{
				Kind: EventToolCallDelta,
				ToolCalls: []OpenAIToolCall{{
					Index: tcIdx,
					Type:  "function",
					Function: OpenAIToolCallFunction{
						Arguments: ev.Delta.PartialJSON,
					},
				}},
			}}, false, "", nil, nil
		case "thinking_delta":
			return []Event{{
				Kind:          EventReasoningDelta,
				Text:          ev.Delta.Thinking,
				ReasoningKind: "text",
			}}, false, "", nil, nil
		default:
			return nil, false, "", nil, nil
		}

	case "content_block_stop":
		// When an Anthropic thinking block ends, emit a reasoning-finished
		// event so the renderer closes the synthetic envelope. The
		// adapter mapper calls adapterrender.ExtractSyntheticParts on
		// the return trip and materializes each kind per provider config:
		// Anthropic emits a native thinking content block; Codex drops by
		// default. The cached prefix stays byte-stable across turns.
		if t.currentBlockType == "thinking" {
			t.currentBlockType = ""
			return []Event{{Kind: EventReasoningFinished}}, false, "", nil, nil
		}
		t.currentBlockType = ""
		return nil, false, "", nil, nil

	case "message_delta":
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

	case "message_stop":
		reason := finishreason.FromAnthropicStream(t.lastStopReason)
		u := &OpenAIUsage{
			PromptTokens:     t.pendingInputTokens,
			CompletionTokens: t.lastOutputTokens,
			TotalTokens:      t.pendingInputTokens + t.lastOutputTokens,
		}
		var extra []Event
		if t.lastStopReason == "refusal" && t.visibleText.Len() > 0 {
			extra = append(extra, Event{Kind: adapterrender.EventAssistantRefusalDelta, Text: t.visibleText.String()})
		}
		return extra, true, reason, u, nil

	case "ping":
		return nil, false, "", nil, nil

	case "error":
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

	default:
		return nil, false, "", nil, nil
	}
}
