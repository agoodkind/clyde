package codex

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/codexwire"
)

// ClientMetadata is the typed `client_metadata` map codex-cli sends on
// the Responses request body. The keys mirror the `x-codex-*` /
// `x-openai-*` constants in research/codex/codex-rs/core/src/client.rs
// (build_ws_client_metadata) and the trace keys added by
// response_create_client_metadata in
// research/codex/codex-rs/codex-api/src/common.rs. Each field carries
// `omitempty` so an absent key is skipped, matching the HashMap codex-cli
// only populates when the value is present.
type ClientMetadata struct {
	InstallationID  string `json:"x-codex-installation-id,omitempty"`
	WindowID        string `json:"x-codex-window-id,omitempty"`
	Subagent        string `json:"x-openai-subagent,omitempty"`
	ParentThreadID  string `json:"x-codex-parent-thread-id,omitempty"`
	TurnMetadata    string `json:"x-codex-turn-metadata,omitempty"`
	WSStreamStartMS string `json:"x-codex-ws-stream-request-start-ms,omitempty"`
	Traceparent     string `json:"ws_request_header_traceparent,omitempty"`
	Tracestate      string `json:"ws_request_header_tracestate,omitempty"`
}

// clientMetadataZero is the zero-value ClientMetadata used for the
// empty-comparison in IsEmpty without constructing a composite literal.
var clientMetadataZero ClientMetadata

// IsEmpty reports whether the metadata carries no populated keys, so the
// caller can decide to omit the whole `client_metadata` field. A nil
// receiver is treated as empty.
func (m *ClientMetadata) IsEmpty() bool {
	return m == nil || *m == clientMetadataZero
}

// TurnMetadataValue returns the `x-codex-turn-metadata` blob nil-safely,
// so transport configs can mirror it into the turn-metadata header
// without dereferencing a possibly-nil metadata pointer.
func (m *ClientMetadata) TurnMetadataValue() string {
	if m == nil {
		return ""
	}
	return m.TurnMetadata
}

// ToMap projects the typed metadata into the string map the websocket
// transport marshal envelope consumes. Only populated keys are emitted,
// using the same `x-codex-*` / `ws_request_header_*` JSON keys codex-cli
// uses on the wire. Returns nil when the metadata is absent or empty.
func (m *ClientMetadata) ToMap() map[string]string {
	if m == nil || m.IsEmpty() {
		return nil
	}
	out := map[string]string{}
	putIfPresent := func(key, value string) {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	putIfPresent("x-codex-installation-id", m.InstallationID)
	putIfPresent("x-codex-window-id", m.WindowID)
	putIfPresent("x-openai-subagent", m.Subagent)
	putIfPresent("x-codex-parent-thread-id", m.ParentThreadID)
	putIfPresent("x-codex-turn-metadata", m.TurnMetadata)
	putIfPresent("x-codex-ws-stream-request-start-ms", m.WSStreamStartMS)
	putIfPresent("ws_request_header_traceparent", m.Traceparent)
	putIfPresent("ws_request_header_tracestate", m.Tracestate)
	if len(out) == 0 {
		return nil
	}
	return out
}

// HTTPTransportRequest is the wire shape every Codex transport
// accepts. The websocket transport reads it via
// ResponseCreateRequestFromHTTP before sending. The name retains the
// historical "HTTP" prefix because BuildRequest, callers in
// continuation.go, and request serialization still use that name.
//
// Field order matches codex-rs ResponsesApiRequest
// (research/codex/codex-rs/codex-api/src/common.rs ~170) exactly,
// because Go encodes JSON in struct-field order, so field order on the
// struct is wire order. The omitempty decisions also match the source:
// `instructions` is skipped when empty (String::is_empty); `tools`,
// `tool_choice`, `parallel_tool_calls`, `reasoning`, and `include` carry
// NO omitempty so they always serialize exactly as codex-cli sends them
// (`"tools":[]`, `"include":[]`, `"reasoning":null` when the pointer is
// nil, etc.).
type HTTPTransportRequest struct {
	Model        string                `json:"model"`
	Instructions string                `json:"instructions,omitempty"`
	Input        []codexwire.InputItem `json:"input"`
	Tools        []codexwire.ToolSpec  `json:"tools"`
	ToolChoice   string                `json:"tool_choice"`
	// ParallelToolCalls has no omitempty so the field always serializes,
	// including the `false` zero value, matching codex-rs which has no
	// skip_serializing_if on parallel_tool_calls.
	ParallelToolCalls bool `json:"parallel_tool_calls"`
	// Reasoning has no omitempty: codex-rs serializes `reasoning` even
	// when the Option is None (`"reasoning":null`).
	Reasoning *Reasoning `json:"reasoning"`
	Store     bool       `json:"store"`
	Stream    bool       `json:"stream"`
	// Include has no omitempty so an empty slice serializes as
	// `"include":[]`, matching codex-rs Vec<String> with no
	// skip_serializing_if.
	Include         []string        `json:"include"`
	ServiceTier     string          `json:"service_tier,omitempty"`
	PromptCache     string          `json:"prompt_cache_key,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Text            json.RawMessage `json:"text,omitempty"`
	ClientMetadata  *ClientMetadata `json:"client_metadata,omitempty"`
	// WebsocketSessionKey is deliberately excluded from the upstream
	// request body. It is Clyde's local identity for websocket
	// connection reuse and must only come from a strong Cursor/Codex
	// conversation or thread id. It is NOT previous_response_id: the
	// codex path runs with store=false (ChatGPT-Pro auth), so the
	// upstream never persists a response and a previous_response_id
	// reference returns "not found". See request_builder.go for the
	// store=false comment. Do not populate this from prompt_cache_key,
	// account user, or content fingerprints: those are cache identities,
	// not live-session identities, and can be shared by unrelated chats.
	WebsocketSessionKey string `json:"-"`
}
