package anthropicbackend

import (
	"encoding/json"
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

// AnthContentBlock is a union of Anthropic content block shapes.
//
// Signature carries the opaque server-signed value Anthropic emits per
// thinking block via the `signature_delta` SSE event. The mapper preserves
// it on the round trip so a replayed thinking block on a later turn passes
// Anthropic's signature validation.
//
// Data carries the opaque base64 payload Anthropic emits inline on a
// `redacted_thinking` content block start event. The wire field name on
// Anthropic is `data`. The mapper preserves it on the round trip so a
// replayed redacted_thinking block on a later turn passes Anthropic's
// history validation. Empty for every other block type.
type AnthContentBlock struct {
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

// AnthSSEEvent is a parsed streaming SSE payload (event type in the enclosing frame).
type AnthSSEEvent struct {
	Type         string              `json:"type"`
	Index        int                 `json:"index"`
	ContentBlock *AnthContentBlock   `json:"content_block,omitempty"`
	Delta        *AnthSSEDelta       `json:"delta,omitempty"`
	Message      *AnthSSEMessageWrap `json:"message,omitempty"`
	Usage        *AnthSSEUsage       `json:"usage,omitempty"`
}

// AnthSSEMessageWrap is the inner message on message_start.
type AnthSSEMessageWrap struct {
	ID         string        `json:"id"`
	Model      string        `json:"model"`
	Role       string        `json:"role"`
	Usage      *AnthSSEUsage `json:"usage,omitempty"`
	StopReason string        `json:"stop_reason,omitempty"`
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
