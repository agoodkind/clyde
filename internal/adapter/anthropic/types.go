// Package anthropic implements Anthropic wire models and helpers.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic/anthmode"
)

// MaxOutputTokens is the upper bound the adapter requests when the
// caller doesn't pin one. The messages API requires max_tokens; OpenAI's
// API defaults to "model max" which we approximate generously.
const MaxOutputTokens = 8192

// WireCaptureMode is the closed enum the anthropic client honors when the
// dispatcher passes a per-provider wire-capture lever. Aliased from the
// leaf [anthmode] package so the parent provider package and the
// [internal/config] package share a single canonical type without an import
// cycle.
type WireCaptureMode = anthmode.WireCaptureMode

// Anthropic wire-capture modes. Off is the safe default and matches an empty
// configured value. Aliased from [anthmode] so callers may continue to use
// the unqualified `anthropic.WireCapture*` names.
const (
	WireCaptureOff         = anthmode.WireCaptureOff
	WireCaptureSummaryOnly = anthmode.WireCaptureSummaryOnly
	WireCaptureFull        = anthmode.WireCaptureFull
)

// InboundThinking is the closed enum of strategies for the visible thinking
// content block round-trip on the inbound (request-shaping) side. Aliased
// from the leaf [anthmode] package so the parent provider package and the
// [internal/config] package share a single canonical type without an import
// cycle.
type InboundThinking = anthmode.InboundThinking

// Anthropic inbound-thinking strategies. Aliased from [anthmode] so callers
// may continue to use the unqualified `anthropic.InboundThinking*` names.
const (
	InboundThinkingNative      = anthmode.InboundThinkingNative
	InboundThinkingDrop        = anthmode.InboundThinkingDrop
	InboundThinkingPlainText   = anthmode.InboundThinkingPlainText
	InboundThinkingPassthrough = anthmode.InboundThinkingPassthrough
)

// Config carries wire header and body-side values for the messages
// API. Populated from the user's config; callers validate before New.
type Config struct {
	MessagesURL             string
	OAuthAnthropicVersion   string
	BetaHeader              string
	UserAgent               string
	SystemPromptPrefix      string
	StainlessPackageVersion string
	StainlessRuntime        string
	StainlessRuntimeVersion string
	CCVersion               string
	CCEntrypoint            string
	// WireCaptureMode controls the optional per-success-path log of the
	// upstream response shape. WireCaptureOff (default) is safe; the
	// other modes route to adapter.providers.anthropic.wire_capture for
	// short-retention diagnostics.
	WireCaptureMode WireCaptureMode
}

// Client wraps an http.Client and an oauth.Manager.
type Client struct {
	http  *http.Client
	oauth OAuthSource
	cfg   Config
}

// OAuthSource is the minimum surface anthropic.Client needs from
// the oauth manager. Defined as a small interface so callers can
// swap it for a fake in tests.
type OAuthSource interface {
	Token(ctx context.Context) (string, error)
}

// CacheControl marks a block / tool / system element as a prompt
// cache breakpoint. Omitted entirely when nil; when set, the server
// treats everything up to and including this element as cacheable.
// TTL is optional: empty means 5 minutes; "1h" requires the
// extended-cache-ttl-2025-04-11 beta.
// Scope widens the cache key beyond the current org:
//   - "" (default): session-scoped, per-OAuth-token.
//   - "global": Anthropic-wide shared cache, eligible on accounts
//     that GrowthBook allowlists. Requires the
//     prompt-caching-scope-2026-01-05 beta.
//   - "org": organization-wide. Same beta.
//
// Only set scope when matching the live CLI wire shape; it changes
// the cache key and can mask bugs in breakpoint placement.
type CacheControl struct {
	Type  string `json:"type"`
	TTL   string `json:"ttl,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// SystemBlock is one element of the typed array form of the system
// prompt. The API accepts system as either a plain string or an
// array of typed text blocks. The array form is required when any
// element carries a cache_control marker.
type SystemBlock struct {
	Type         string        `json:"type"` // always "text" today
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ContentBlock is one element in a message content array or a streamed
// content block start payload subset we reuse for decoding.
//
// Signature carries the opaque per-thinking-block signature Anthropic
// emits via the `signature_delta` SSE event. On replay the wire
// representation must include `"signature":"<value>"` alongside
// `"thinking":"<body>"` so Anthropic's signature validation passes.
//
// Data carries the opaque base64 payload Anthropic emits inline on a
// `redacted_thinking` content block start event. On replay the wire
// representation must include `"data":"<value>"` alongside
// `"type":"redacted_thinking"` so Anthropic's history validation
// passes.
type ContentBlock struct {
	Type           string          `json:"type"`
	Text           string          `json:"text,omitempty"`
	ID             string          `json:"id,omitempty"`
	Name           string          `json:"name,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	ToolUseID      string          `json:"tool_use_id,omitempty"`
	Content        string          `json:"content,omitempty"`
	CacheReference string          `json:"cache_reference,omitempty"`
	Source         *ImageSource    `json:"source,omitempty"`
	CacheControl   *CacheControl   `json:"cache_control,omitempty"`
	Thinking       string          `json:"thinking,omitempty"`
	Signature      string          `json:"signature,omitempty"`
	Data           string          `json:"data,omitempty"`
}

// ImageSource describes image bytes or a URL for image content blocks.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Message is one entry in the /v1/messages "messages" array.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

func (m *Message) UnmarshalJSON(raw []byte) error {
	type messageWire struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	var wire messageWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	content, err := decodeContentBlocks(wire.Content)
	if err != nil {
		return err
	}
	m.Role = wire.Role
	m.Content = content
	return nil
}

// MarshalJSON emits a string "content" when there is exactly one text
// block; otherwise it emits a JSON array of blocks. Both shapes are
// accepted by the server; the string form preserves prompt-cache
// behavior for the common plain-text path. When the single block
// carries a CacheControl marker the array shape is forced so the
// marker survives serialization.
func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Content) == 1 && m.Content[0].Type == "text" && m.Content[0].CacheControl == nil {
		return json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{
			Role:    m.Role,
			Content: m.Content[0].Text,
		})
	}
	return json.Marshal(struct {
		Role    string         `json:"role"`
		Content []ContentBlock `json:"content"`
	}{
		Role:    m.Role,
		Content: m.Content,
	})
}

// Tool is the wire shape for the tools array on /v1/messages.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *CacheControl   `json:"cache_control,omitempty"`
}

// ToolChoice selects how the model may use tools.
type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// MetadataUserID is the typed payload claude-cli serializes into
// metadata.user_id. The wire form is a JSON string (the inner object
// is JSON-encoded twice). Mirrors the captured shape in
// research/claude-code/snapshots/latest/reference.toml.
type MetadataUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// RequestMetadata is the metadata.* block on /v1/messages.
type RequestMetadata struct {
	UserID string `json:"user_id"`
}

// ContextManagementEdit is one edit instruction. Only
// clear_thinking_20251015 is observed today.
type ContextManagementEdit struct {
	Type string `json:"type"`
	Keep string `json:"keep,omitempty"`
}

// ContextManagement carries the context_management block.
type ContextManagement struct {
	Edits []ContextManagementEdit `json:"edits,omitempty"`
}

// Request is the subset of the /v1/messages body we generate.
// System and SystemBlocks are mutually exclusive on the wire; the
// custom MarshalJSON emits SystemBlocks as "system":[...] when
// populated, otherwise falls back to the plain string form for
// back-compat. Callers building typed system blocks should set
// SystemBlocks and leave System empty.
type Request struct {
	Model        string        `json:"model"`
	System       string        `json:"system,omitempty"`
	SystemBlocks []SystemBlock `json:"-"`
	Messages     []Message     `json:"messages"`
	MaxTokens    int           `json:"max_tokens"`
	Stream       bool          `json:"stream"`
	OutputConfig *OutputConfig `json:"output_config,omitempty"`
	// Thinking selects extended thinking. Three shapes are accepted
	// by the API: {type:"adaptive"} (no budget), {type:"enabled",
	// budget_tokens:N}, and {type:"disabled"}. Adaptive thinking is
	// rejected on haiku-4-5 with
	// `400 adaptive thinking is not supported on this model`;
	// enabled requires `max_tokens > budget_tokens`.
	Thinking          *Thinking          `json:"thinking,omitempty"`
	Tools             []Tool             `json:"tools,omitempty"`
	ToolChoice        *ToolChoice        `json:"tool_choice,omitempty"`
	Metadata          *RequestMetadata   `json:"metadata,omitempty"`
	ContextManagement *ContextManagement `json:"context_management,omitempty"`
	// OnHeaders receives the raw response headers on successful 200 responses.
	// The callback executes during do() after the /v1/messages request is
	// committed and before status-specific processing.
	OnHeaders func(http.Header) `json:"-"`
	// ExtraBetas is appended to cfg.BetaHeader when building the
	// outbound anthropic-beta header. Use for per-model / per-request
	// flags the static config does not already include. Not serialized.
	ExtraBetas []string `json:"-"`
}

func (r *Request) UnmarshalJSON(raw []byte) error {
	type requestWire struct {
		Model             string             `json:"model"`
		System            json.RawMessage    `json:"system"`
		Messages          []Message          `json:"messages"`
		MaxTokens         int                `json:"max_tokens"`
		Stream            bool               `json:"stream"`
		OutputConfig      *OutputConfig      `json:"output_config,omitempty"`
		Thinking          *Thinking          `json:"thinking,omitempty"`
		Tools             []Tool             `json:"tools,omitempty"`
		ToolChoice        *ToolChoice        `json:"tool_choice,omitempty"`
		Metadata          *RequestMetadata   `json:"metadata,omitempty"`
		ContextManagement *ContextManagement `json:"context_management,omitempty"`
	}
	var wire requestWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	system, blocks, err := decodeSystemPrompt(wire.System)
	if err != nil {
		return err
	}
	r.Model = wire.Model
	r.System = system
	r.SystemBlocks = blocks
	r.Messages = wire.Messages
	r.MaxTokens = wire.MaxTokens
	r.Stream = wire.Stream
	r.OutputConfig = wire.OutputConfig
	r.Thinking = wire.Thinking
	r.Tools = wire.Tools
	r.ToolChoice = wire.ToolChoice
	r.Metadata = wire.Metadata
	r.ContextManagement = wire.ContextManagement
	return nil
}

// MarshalJSON emits SystemBlocks as an array under "system" when
// present so typed blocks with cache_control markers land on the
// wire. Falls back to the plain string form otherwise.
func (r Request) MarshalJSON() ([]byte, error) {
	type alias Request
	base := alias(r)
	base.SystemBlocks = nil
	if len(r.SystemBlocks) == 0 {
		return json.Marshal(base)
	}
	// SystemBlocks wins. Clear the string form on the wire so we do
	// not double-emit system.
	base.System = ""
	encoded, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	// Splice "system":<blocks> into the encoded object. This is
	// cheaper than a second struct definition mirroring every field.
	blocks, err := json.Marshal(r.SystemBlocks)
	if err != nil {
		return nil, err
	}
	insertion := []byte(`"system":` + string(blocks) + `,`)
	// Insert just after the opening brace.
	if len(encoded) < 2 || encoded[0] != '{' {
		return nil, fmt.Errorf("anthropic: marshaled request is not a JSON object: %q", encoded)
	}
	out := make([]byte, 0, len(encoded)+len(insertion))
	out = append(out, '{')
	out = append(out, insertion...)
	out = append(out, encoded[1:]...)
	return out, nil
}

// OutputConfig is the wire shape that wraps effort and (later) other
// per-request output knobs. Effort must nest under this object for
// the server to accept it when the beta header allows effort.
type OutputConfig struct {
	// Effort is one of low/medium/high/max. The adapter only sets
	// this when the registry says the family supports it; haiku-4-5
	// returns `400 This model does not support the effort parameter`.
	Effort string `json:"effort,omitempty"`
}

// Thinking is the optional extended-thinking knob. BudgetTokens uses
// `omitempty` so the adaptive shape doesn't serialize a stray
// `"budget_tokens":0` (which the API rejects).
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

// Usage mirrors the prompt/completion token counts from the response.
// CacheCreationInputTokens is the count of input tokens written into
// the prompt cache on this request; CacheReadInputTokens is the count
// served from cache. Both are zero when prompt caching is disabled.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// Sink receives a single text delta chunk during streaming. Empty
// chunks are skipped.
type Sink func(delta string) error

// StreamEvent is a decoded streaming signal from /v1/messages SSE.
//
// Kind enumerates the signals the parser surfaces:
//   - "text"               assistant visible text delta; Text carries the chunk.
//   - "thinking"            thinking content block open or text delta;
//     Text empty on open, populated on subsequent thinking_delta events.
//   - "thinking_signature"  per-thinking-block signature delta; Text carries
//     the opaque signature emitted by Anthropic's signature_delta event.
//   - "redacted_thinking"   redacted_thinking content block; Data carries
//     the opaque base64 payload Anthropic emits inline on the start event.
//     There is no separate delta or close-side payload for this kind.
//   - "tool_use_start"      tool_use content block open; ToolUseID and
//     ToolUseName carry the call identity.
//   - "tool_use_arg_delta"  tool_use partial JSON arguments; PartialJSON
//     carries the chunk.
//   - "tool_use_stop"       tool_use content block close.
//   - "stop"                terminal message stop; StopReason is set.
type StreamEvent struct {
	Kind        string
	Text        string
	BlockIndex  int
	ToolUseID   string
	ToolUseName string
	PartialJSON string
	StopReason  string
	Data        string
}

// EventSink receives structured stream events.
type EventSink func(StreamEvent) error

// Result is the aggregated outcome of a non-streaming call.
type Result struct {
	Text  string
	Usage Usage
	Stop  string
}

type MessageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Content      []ContentBlock `json:"content"`
	Model        string         `json:"model"`
	StopReason   string         `json:"stop_reason,omitempty"`
	StopSequence string         `json:"stop_sequence,omitempty"`
	Usage        ResponseUsage  `json:"usage"`
}

type ResponseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type ErrorEnvelope struct {
	Type  string      `json:"type"`
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

func decodeContentBlocks(raw json.RawMessage) ([]ContentBlock, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		return []ContentBlock{{
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
		}}, nil
	}
	if trimmed[0] == '[' {
		var blocks []ContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, err
		}
		for i := range blocks {
			if strings.TrimSpace(blocks[i].Type) == "" {
				blocks[i].Type = "text"
			}
		}
		return blocks, nil
	}
	return nil, fmt.Errorf("anthropic: content must be a string or array")
}

func decodeSystemPrompt(raw json.RawMessage) (string, []SystemBlock, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", nil, err
		}
		return text, nil, nil
	}
	if trimmed[0] == '[' {
		var blocks []SystemBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return "", nil, err
		}
		for i := range blocks {
			if strings.TrimSpace(blocks[i].Type) == "" {
				blocks[i].Type = "text"
			}
		}
		return "", blocks, nil
	}
	return "", nil, fmt.Errorf("anthropic: system must be a string or array")
}
