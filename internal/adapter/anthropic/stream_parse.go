// Package anthropic implements Anthropic wire models and helpers.
package anthropic

import (
	"encoding/json"
	"fmt"
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
	switch eventName {
	case "ping":
		return nil
	case "message_start":
		var ev streamMessageStartEvent
		if err := json.Unmarshal([]byte(data), &ev); err == nil {
			usage.InputTokens = ev.Message.Usage.InputTokens
			usage.OutputTokens = ev.Message.Usage.OutputTokens
			usage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
			usage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
		}
	case "content_block_start":
		var ev streamContentBlockStartEvent
		if err := json.Unmarshal([]byte(data), &ev); err == nil {
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
				return sink(StreamThinkingStart{
					BlockIndex: ev.Index,
				})
			case streamContentBlockTypeRedactedThinking:
				// Anthropic emits the opaque payload on the start event
				// itself. There is no redacted_thinking_delta; we surface
				// one event per block carrying the data blob and rely on
				// content_block_stop for closing.
				return sink(StreamRedactedThinking{
					BlockIndex: ev.Index,
					Data:       ev.ContentBlock.Data,
				})
			case streamContentBlockTypeText:
				// Plain text content blocks are observed via the
				// per-delta path; no synchronous event is needed at
				// start. Listed here so the enum switch is exhaustive
				// for the canonical Anthropic block types.
			}
		}
	case "content_block_delta":
		var ev streamContentBlockDeltaEvent
		if err := json.Unmarshal([]byte(data), &ev); err == nil {
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text == "" {
					return nil
				}
				return sink(StreamTextDelta{
					BlockIndex: ev.Index,
					Text:       ev.Delta.Text,
				})
			case "input_json_delta":
				// Anthropic emits a leading content_block_delta with
				// an empty partial_json to "open" the tool input
				// stream. Forwarding it produces a tool_call delta with
				// a zero-value Function block that json:"function,omitzero"
				// drops, leaving a bare {"index":N,"type":"function"}
				// chunk on the wire. Cursor's OpenAI SSE parser treats
				// that as a finalize/reset and drops the whole tool
				// call, so the user sees no Read/Glob card.
				if ev.Delta.PartialJSON == "" {
					return nil
				}
				return sink(StreamToolUseArgDelta{
					BlockIndex:  ev.Index,
					PartialJSON: ev.Delta.PartialJSON,
				})
			case "thinking_delta":
				return sink(StreamThinkingDelta{
					BlockIndex: ev.Index,
					Text:       ev.Delta.Thinking,
				})
			case "signature_delta":
				return sink(StreamThinkingSignature{
					BlockIndex: ev.Index,
					Signature:  ev.Delta.Signature,
				})
			}
		}
	case "content_block_stop":
		var ev streamContentBlockStopEvent
		if err := json.Unmarshal([]byte(data), &ev); err == nil {
			if blockTypes[ev.Index] == "tool_use" {
				delete(blockTypes, ev.Index)
				return sink(StreamToolUseStop{
					BlockIndex: ev.Index,
				})
			}
			delete(blockTypes, ev.Index)
		}
	case "message_delta":
		var ev streamMessageDeltaEvent
		if err := json.Unmarshal([]byte(data), &ev); err == nil {
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
	case "message_stop":
		return sink(StreamStop{
			StopReason: *stop,
		})
	case "error":
		var ev streamErrorEvent
		if err := json.Unmarshal([]byte(data), &ev); err == nil {
			return fmt.Errorf("anthropic error: %s: %s", ev.Error.Type, ev.Error.Message)
		}
		return fmt.Errorf("anthropic error: %s", truncate(data, 400))
	}
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
