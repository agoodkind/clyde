package anthropicbackend

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// AnthRequest is the translated Anthropic messages request.
type AnthRequest struct {
	Model      string          `json:"model"`
	System     string          `json:"system,omitempty"`
	Messages   []AnthMessage   `json:"messages"`
	MaxTokens  int             `json:"max_tokens"`
	Tools      []AnthTool      `json:"tools,omitempty"`
	ToolChoice *AnthToolChoice `json:"tool_choice,omitempty"`
	Stream     bool            `json:"stream,omitempty"`
}

// AnthMessage is one conversation turn.
type AnthMessage struct {
	Role    string             `json:"-"`
	Content []AnthContentBlock `json:"-"`
}

// AnthContentBlock is the sealed marker interface for Anthropic content block
// variants. The six concrete types are TextBlock, ImageBlock, ToolUseBlock,
// ToolResultBlock, ThinkingBlock, and RedactedThinkingBlock. Consumers
// type-switch on the concrete type.
type AnthContentBlock interface {
	isAnthContentBlock()
	// blockType returns the Anthropic wire type string for this block.
	blockType() string
}

// TextBlock is an Anthropic "text" content block.
type TextBlock struct {
	Text string
}

func (TextBlock) isAnthContentBlock() {}
func (TextBlock) blockType() string   { return "text" }

// ImageBlock is an Anthropic "image" content block.
type ImageBlock struct {
	Source *AnthImageSource
}

func (ImageBlock) isAnthContentBlock() {}
func (ImageBlock) blockType() string   { return "image" }

// ToolUseBlock is an Anthropic "tool_use" content block.
type ToolUseBlock struct {
	ID    string
	Name  string
	Input json.RawMessage
}

func (ToolUseBlock) isAnthContentBlock() {}
func (ToolUseBlock) blockType() string   { return "tool_use" }

// ToolResultBlock is an Anthropic "tool_result" content block.
type ToolResultBlock struct {
	ToolUseID     string
	ResultContent string
}

func (ToolResultBlock) isAnthContentBlock() {}
func (ToolResultBlock) blockType() string   { return "tool_result" }

// ThinkingBlock is an Anthropic "thinking" content block.
//
// Signature carries the opaque server-signed value Anthropic emits per
// thinking block via the `signature_delta` SSE event. The mapper preserves
// it on the round trip so a replayed thinking block on a later turn passes
// Anthropic's signature validation.
type ThinkingBlock struct {
	Thinking  string
	Signature string
}

func (ThinkingBlock) isAnthContentBlock() {}
func (ThinkingBlock) blockType() string   { return "thinking" }

// RedactedThinkingBlock is an Anthropic "redacted_thinking" content block.
//
// Data carries the opaque base64 payload Anthropic emits inline on a
// `redacted_thinking` content block start event. The wire field name on
// Anthropic is `data`. The mapper preserves it on the round trip so a
// replayed redacted_thinking block on a later turn passes Anthropic's
// history validation.
type RedactedThinkingBlock struct {
	Data string
}

func (RedactedThinkingBlock) isAnthContentBlock() {}
func (RedactedThinkingBlock) blockType() string   { return "redacted_thinking" }

// blockTypeStr is the Anthropic wire discriminator for content block types.
type blockTypeStr string

const (
	blockTypeText             blockTypeStr = "text"
	blockTypeImage            blockTypeStr = "image"
	blockTypeToolUse          blockTypeStr = "tool_use"
	blockTypeToolResult       blockTypeStr = "tool_result"
	blockTypeThinking         blockTypeStr = "thinking"
	blockTypeRedactedThinking blockTypeStr = "redacted_thinking"
)

// rawContentBlock is the concrete struct used for JSON decoding inbound
// Anthropic SSE content_block_start payloads. Callers call rawContentBlock.typed()
// to obtain the typed AnthContentBlock variant.
type rawContentBlock struct {
	Type          string           `json:"type"`
	Text          string           `json:"text,omitempty"`
	ID            string           `json:"id,omitempty"`
	Name          string           `json:"name,omitempty"`
	Input         json.RawMessage  `json:"input,omitempty"`
	ToolUseID     string           `json:"tool_use_id,omitempty"`
	ResultContent string           `json:"content,omitempty"`
	Source        *AnthImageSource `json:"source,omitempty"`
	Thinking      string           `json:"thinking,omitempty"`
	Signature     string           `json:"signature,omitempty"`
	Data          string           `json:"data,omitempty"`
}

// typed converts a raw decoded block into the appropriate typed variant.
// Returns (nil, false) for unknown type strings.
func (r rawContentBlock) typed() (AnthContentBlock, bool) {
	switch blockTypeStr(r.Type) {
	case blockTypeText:
		return TextBlock{Text: r.Text}, true
	case blockTypeImage:
		return ImageBlock{Source: r.Source}, true
	case blockTypeToolUse:
		return ToolUseBlock{ID: r.ID, Name: r.Name, Input: r.Input}, true
	case blockTypeToolResult:
		return ToolResultBlock{ToolUseID: r.ToolUseID, ResultContent: r.ResultContent}, true
	case blockTypeThinking:
		return ThinkingBlock{Thinking: r.Thinking, Signature: r.Signature}, true
	case blockTypeRedactedThinking:
		return RedactedThinkingBlock{Data: r.Data}, true
	default:
		return nil, false
	}
}

// UnmarshalContentBlock JSON-decodes one Anthropic wire content block into a
// typed AnthContentBlock variant. Returns nil and a nil error for an empty
// JSON object or unknown type strings; callers that require a non-nil result
// must check the bool ok return.
func UnmarshalContentBlock(data []byte) (AnthContentBlock, bool, error) {
	var raw rawContentBlock
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("adapter.anthropic.content_block.unmarshal_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return nil, false, fmt.Errorf("decode content block: %w", err)
	}
	if raw.Type == "" {
		return nil, false, nil
	}
	block, typed := raw.typed()
	// Unknown type: silently skip so new upstream block types do not
	// break the stream. The caller's switch falls to default.
	if !typed {
		return nil, false, nil
	}
	return block, true, nil
}

// AnthImageSource describes image bytes or a remote URL.
type AnthImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthTool is one tool definition.
type AnthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AnthToolChoice mirrors Anthropic tool_choice.
type AnthToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// AnthSSEDelta is the delta object on content_block_delta and message_delta.
//
// Signature is populated when the upstream emits a `signature_delta` event
// inside a thinking content block. The value is the opaque server-signed
// signature for that thinking block.
type AnthSSEDelta struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	Thinking     string `json:"thinking,omitempty"`
	PartialJSON  string `json:"partial_json,omitempty"`
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
	Signature    string `json:"signature,omitempty"`
}

// AnthSSEMessage is used on message_delta wrapper when needed.
type AnthSSEMessage struct {
	ID         string       `json:"id"`
	Model      string       `json:"model"`
	Usage      AnthSSEUsage `json:"usage"`
	StopReason string       `json:"stop_reason,omitempty"`
}

// AnthSSEUsage is token accounting in stream events.
type AnthSSEUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthSSEMessageWrap is the inner message on message_start.
type AnthSSEMessageWrap struct {
	ID         string        `json:"id"`
	Model      string        `json:"model"`
	Role       string        `json:"role"`
	Usage      *AnthSSEUsage `json:"usage,omitempty"`
	StopReason string        `json:"stop_reason,omitempty"`
}
