package config

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic/anthmode"
)

// Config represents the clyde configuration.
type Config struct {
	// Defaults are applied to all sessions unless overridden
	Defaults Defaults `json:"defaults" toml:"defaults"`
	// Profiles is a map of named session profiles
	Profiles map[string]Profile `json:"profiles,omitempty" toml:"profiles,omitempty"`
	// Logging configures process-wide runtime behavior.
	Logging LoggingConfig `json:"logging" toml:"logging"`
	// Search configures the conversation search LLM backend
	Search SearchConfig `json:"search" toml:"search"`
	// Adapter configures the OpenAI compatible HTTP adapter mounted
	// inside the daemon process.
	Adapter AdapterConfig `json:"adapter" toml:"adapter"`
	// WebApp configures the optional remote dashboard mounted by the
	// daemon. The dashboard exposes a small HTML form plus a JSON API
	// for spawning new remote control sessions and lists every active
	// bridge URL. Pair with cloudflared to expose securely.
	WebApp WebAppConfig `json:"webApp" toml:"web_app"`
	// Prune configures the daemon's periodic session pruning loop.
	// Disabled by default so existing installs see no behavior change
	// until the user opts in.
	Prune PruneConfig `json:"prune" toml:"prune"`
	// OAuth configures the daemon's background OAuth token refresher.
	// The refresher keeps a warm access token in the keychain
	// so the adapter direct-OAuth path almost never has to refresh
	// inline.
	OAuth OAuthConfig `json:"oauth" toml:"oauth"`
	// Labeler configures the per-session topic labeler that writes a
	// short bookmark-style label into Metadata.Context. The previous
	// implementation shelled out to `claude -p --model sonnet`, which
	// recursed through the SessionStart hook chain and fanned out
	// uncontrollably. The shellout has been ripped out; this struct
	// is the wiring point for the eventual rewrite against the
	// in-process adapter. Disabled by default until then.
	Labeler LabelerConfig `json:"labeler" toml:"labeler"`
	// MITM configures the local capture proxy used for provider
	// subprocesses and for adapter-side request observability.
	MITM MITMConfig `json:"mitm" toml:"mitm"`
	// AutoName configures the automatic session-naming worker.
	// CLYDE-170 PR3 adds the parsed config block. The worker that
	// consumes this config lands in PR4. Defaults are applied when
	// the [autoname] block is absent or partial.
	AutoName AutoNameConfig `json:"autoName" toml:"autoname"`
}

// LoggingConfig carries global logging settings.
type LoggingConfig struct {
	Level      string            `json:"level,omitempty" toml:"level,omitempty"`
	Rotation   LoggingRotation   `json:"rotation,omitzero" toml:"rotation,omitempty"`
	RawCapture LoggingToggle     `json:"raw_capture,omitzero" toml:"raw_capture,omitempty"`
	Cleanup    LoggingCleanup    `json:"cleanup,omitzero" toml:"cleanup,omitempty"`
	Request    LoggingRequest    `json:"request,omitzero" toml:"request,omitempty"`
	Inventory  LoggingInventory  `json:"inventory,omitzero" toml:"inventory,omitempty"`
	Paths      LoggingPaths      `json:"paths,omitzero" toml:"paths,omitempty"`
	Transcript LoggingTranscript `json:"transcript,omitzero" toml:"transcript,omitempty"`
	Sinks      LoggingSinks      `json:"sinks,omitzero" toml:"sinks,omitempty"`
	Concerns   LoggingConcerns   `json:"concerns,omitempty" toml:"concerns,omitempty"`
}

// LoggingSinks carries the enabled central sink names.
type LoggingSinks struct {
	Enabled []string `json:"enabled,omitempty" toml:"enabled,omitempty"`
}

// LoggingConcerns maps registered concern names to per-concern overrides.
type LoggingConcerns map[string]LoggingConcern

const (
	// LoggingSinkDaemon names the daemon process log sink.
	LoggingSinkDaemon = "daemon"
	// LoggingSinkTUI names the TUI process log sink.
	LoggingSinkTUI = "tui"
	// LoggingSinkCodexSidecar names the Codex sidecar log sink.
	LoggingSinkCodexSidecar = "codex_sidecar"
	// LoggingSinkAnthropicSidecar names the Anthropic sidecar log sink.
	LoggingSinkAnthropicSidecar = "anthropic_sidecar"
	// LoggingSinkAudit names the cross-process audit log sink.
	LoggingSinkAudit = "audit"
	// LoggingSinkConcerns names the structured concern log sink.
	LoggingSinkConcerns = "concerns"
	// LoggingSinkTranscripts names the per-chat transcript sink.
	LoggingSinkTranscripts = "transcripts"
	// LoggingSinkMITMCapture names the MITM capture index sink.
	LoggingSinkMITMCapture = "mitm_capture"
	// LoggingSinkMITMRaw names the MITM raw payload sink.
	LoggingSinkMITMRaw = "mitm_raw"
	// LoggingSinkInventory names the inventory index sink.
	LoggingSinkInventory = "inventory_index"
)

// LoggingConcern carries config-layer controls for a registered concern.
type LoggingConcern struct {
	Enabled  *bool           `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Level    string          `json:"level,omitempty" toml:"level,omitempty"`
	Detail   string          `json:"detail,omitempty" toml:"detail,omitempty"`
	Sink     string          `json:"sink,omitempty" toml:"sink,omitempty"`
	Rotation LoggingRotation `json:"rotation,omitzero" toml:"rotation,omitempty"`
}

// LoggingTranscript controls the per-chat transcript router that tees a
// curated allowlist of records to chats/<chat_key>.jsonl, one file per chat.
// Default: on. The operator turns it off by setting Enabled = false.
type LoggingTranscript struct {
	// Enabled toggles the per-chat router. Default true.
	Enabled *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
}

// IsEnabled reports whether the transcript router should be wired in.
// Defaults to true; operators can flip Enabled = false to disable.
func (t LoggingTranscript) IsEnabled() bool {
	if t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// LoggingRotation controls file rotation behavior for the unified clyde logger.
type LoggingRotation struct {
	Enabled    *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	MaxSizeMB  int   `json:"max_size_mb,omitempty" toml:"max_size_mb,omitempty"`
	MaxBackups int   `json:"max_backups,omitempty" toml:"max_backups,omitempty"`
	MaxAgeDays int   `json:"max_age_days,omitempty" toml:"max_age_days,omitempty"`
	Compress   *bool `json:"compress,omitempty" toml:"compress,omitempty"`
}

// LoggingCleanup controls deletion of old log files separately from file rotation.
type LoggingCleanup struct {
	Enabled    *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	MaxAgeDays *int  `json:"max_age_days,omitempty" toml:"max_age_days,omitempty"`
	MaxBackups *int  `json:"max_backups,omitempty" toml:"max_backups,omitempty"`
	MaxTotalMB *int  `json:"max_total_mb,omitempty" toml:"max_total_mb,omitempty"`
}

// LoggingToggle is a named on/off logging control.
type LoggingToggle struct {
	Enabled *bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
}

// LoggingRequest controls request-story contract checks.
type LoggingRequest struct {
	RequiredLegs     map[string][]string `json:"required_legs,omitempty" toml:"required_legs,omitempty"`
	IncompletePolicy string              `json:"incomplete_policy,omitempty" toml:"incomplete_policy,omitempty"`
}

// LoggingInventory controls how clyde logs inventory discovers log locations.
type LoggingInventory struct {
	Mode string `json:"mode,omitempty" toml:"mode,omitempty"`
}

// LoggingPaths controls per-process JSONL destinations. When a path is empty,
// slogger picks a process-specific default under $XDG_STATE_HOME/clyde.
type LoggingPaths struct {
	TUI    string `json:"tui,omitempty" toml:"tui,omitempty"`
	Daemon string `json:"daemon,omitempty" toml:"daemon,omitempty"`
}

// LabelerConfig drives the (currently stubbed) session topic labeler.
// Enabled is the only knob today; turning it on without a working
// adapter implementation is a no-op and emits a warning log.
type LabelerConfig struct {
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
}

// PruneConfig drives the daemon's periodic session pruning loop. The
// pruner is opt-in. When Enabled the daemon ticks every Interval and
// runs the kinds set to true. Defaults are conservative: ephemeral
// and empty are safe to auto-prune; autoname is left off because that
// pruner is untested at scale.
type PruneConfig struct {
	Enabled        bool          `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Interval       time.Duration `json:"interval,omitempty" toml:"interval,omitempty"`
	Ephemeral      bool          `json:"ephemeral,omitempty" toml:"ephemeral,omitempty"`
	Empty          bool          `json:"empty,omitempty" toml:"empty,omitempty"`
	Autoname       bool          `json:"autoname,omitempty" toml:"autoname,omitempty"`
	EmptyMaxLines  int           `json:"emptyMaxLines,omitempty" toml:"empty_max_lines,omitempty"`
	EmptyMinAge    time.Duration `json:"emptyMinAge,omitempty" toml:"empty_min_age,omitempty"`
	AutonameMinAge time.Duration `json:"autonameMinAge,omitempty" toml:"autoname_min_age,omitempty"`
}

// OAuthConfig drives the daemon's background OAuth refresh goroutine.
// The refresher is opt-out (default on) because the adapter's
// direct-OAuth path depends on a warm access token. Disabled is a
// pointer so we can distinguish "not set" (use default: enabled) from
// an explicit "disabled = true" in TOML.
type OAuthConfig struct {
	// Disabled, when explicitly true, turns the background refresher
	// off. The adapter's inline refresh still works as a safety net.
	// Default behavior (nil or false) is enabled.
	Disabled *bool `json:"disabled,omitempty" toml:"disabled,omitempty"`
	// Interval between refresh attempts. Default 4 hours (half the
	// 8 hour OAuth access token lifetime so a single missed tick still
	// leaves plenty of headroom before expiry).
	Interval time.Duration `json:"interval,omitempty" toml:"interval,omitempty"`
}

// IsEnabled reports whether the background OAuth refresher should
// run. Defaults to true unless the user explicitly set Disabled to
// true in their config.
func (o OAuthConfig) IsEnabled() bool {
	if o.Disabled != nil && *o.Disabled {
		return false
	}
	return true
}

// WebAppConfig configures the optional in daemon web dashboard.
type WebAppConfig struct {
	// Enabled toggles the listener.
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	// Host defaults to [::1].
	Host string `json:"host,omitempty" toml:"host,omitempty"`
	// Port defaults to 11435.
	Port int `json:"port,omitempty" toml:"port,omitempty"`
	// RequireToken, when set, demands matching bearer auth on every
	// request. CLYDE_WEBAPP_TOKEN env override applies.
	RequireToken string `json:"requireToken,omitempty" toml:"require_token,omitempty"`
	// ClydeBinary is the path used to spawn new sessions when the
	// dashboard's "Start" button is invoked. Empty falls back to the
	// daemon's resolved executable name.
	ClydeBinary string `json:"clydeBinary,omitempty" toml:"clyde_binary,omitempty"`
}

// AdapterConfig configures the OpenAI compatible HTTP server folded
// into the clyde daemon monolith. A single launchd entry boots the
// daemon plus this adapter. The default model, port, and per model
// effort matrix live here. User defined entries under Models let
// callers add custom aliases without recompiling.
type AdapterConfig struct {
	// Enabled toggles the HTTP listener. Default is false so the
	// daemon stays headless until the user opts in.
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	// Host defaults to [::1] (loopback only).
	Host string `json:"host,omitempty" toml:"host,omitempty"`
	// Port defaults to 11434 (shared with Ollama conventions).
	Port int `json:"port,omitempty" toml:"port,omitempty"`
	// DefaultModel is the fallback when a request does not name one.
	DefaultModel string `json:"defaultModel,omitempty" toml:"default_model,omitempty"`
	// MaxConcurrent caps the number of in flight claude subprocesses.
	MaxConcurrent int `json:"maxConcurrent,omitempty" toml:"max_concurrent,omitempty"`
	// RequireToken, when set, demands a matching bearer token on
	// every request. The env var CLYDE_ADAPTER_TOKEN overrides.
	RequireToken string `json:"requireToken,omitempty" toml:"require_token,omitempty"`
	// Models lets users add or override adapter model entries.
	// Keys are the public (OpenAI style or real Claude) aliases the
	// client sends. Values name the backend and its tuning knobs.
	Models map[string]AdapterModel `json:"models,omitempty" toml:"models,omitempty"`
	// PassthroughOverrides lets users forward specific aliases to an
	// upstream OpenAI-compatible endpoint.
	PassthroughOverrides map[string]AdapterPassthroughOverride `json:"passthroughOverrides,omitempty" toml:"passthrough_overrides,omitempty"`
	// OpenAICompatPassthrough forwards otherwise-unknown model aliases
	// to a directly configured OpenAI-compatible upstream. Empty
	// BaseURL disables passthrough and unknown aliases 400.
	OpenAICompatPassthrough AdapterOpenAICompatPassthrough `json:"openaiCompatPassthrough,omitzero" toml:"openai_compat_passthrough,omitempty"`
	// DirectOAuth, when true, routes Claude backend requests straight
	// at the configured messages URL using the user's OAuth token from
	// the local keychain.
	DirectOAuth bool `json:"directOauth,omitempty" toml:"direct_oauth,omitempty"`
	// OAuth holds token endpoint, API URLs, and keychain label for the
	// direct-OAuth path and the background token refresher. Required
	// when DirectOAuth is true; also required when the global [oauth]
	// refresher is enabled so periodic refresh can reach the token URL.
	OAuth AdapterOAuth `json:"oauth,omitzero" toml:"oauth,omitempty"`
	// ClientIdentity carries wire request-shape fields (headers and
	// body-side billing line inputs). There are no compiled-in defaults:
	// NewRegistry rejects empty required fields. See clyde.example.toml.
	ClientIdentity AdapterClientIdentity `json:"clientIdentity,omitzero" toml:"client_identity,omitempty"`
	// Logprobs configures per-backend handling of the OpenAI
	// logprobs / top_logprobs request fields. Anthropic does not
	// emit logprobs and `claude -p` does not either; OpenAI-compatible
	// passthrough routes may.
	// There is no compiled-in default. When either backend key is
	// set, NewRegistry requires both keys and rejects unknown values.
	Logprobs AdapterLogprobs `json:"logprobs,omitzero" toml:"logprobs,omitempty"`
	// Notices controls the synthetic notice injection path that annotates
	// assistant turns with overage / budget context in a hidden sentinel.
	// Omitted defaults to true so operators can disable by setting
	// enabled = false.
	Notices AdapterNotices `json:"notices,omitzero" toml:"notices,omitempty"`
	// Families declares the per-family Claude capability matrix the
	// registry expands into the public alias set at load time. Keyed
	// by a stable family slug (e.g. "opus-4-7", "sonnet-4-6",
	// "haiku-4-5"). Empty disables direct-OAuth model resolution.
	Families map[string]AdapterFamily `json:"families,omitempty" toml:"families,omitempty"`
	// Codex configures routing for ChatGPT model names (gpt-*, o*)
	// through the Codex backend-api surface. This keeps Cursor on the
	// same adapter endpoint/port while letting model name choose
	// backend.
	Codex AdapterCodex `json:"codex,omitzero" toml:"codex,omitempty"`
	// Anthropic carries Anthropic-specific sub-blocks introduced after
	// the original AdapterConfig shape stabilized. Existing Anthropic
	// settings (OAuth, ClientIdentity, Notices, etc.) stay as siblings
	// of this struct on AdapterConfig for backward compatibility; new
	// per-provider sub-blocks live here so callers see one canonical
	// [adapter.anthropic] root.
	Anthropic AdapterAnthropic `json:"anthropic,omitzero" toml:"anthropic,omitempty"`
	// WireCapture holds the shared rotation budget for upstream wire
	// body logging. Per-provider mode levers live on each provider's
	// sub-block (AdapterAnthropic.WireCapture, AdapterCodex.WireCapture).
	// Rotation is shared because it is a filesystem concern, not a
	// per-provider one; mode is per-provider because legal values differ.
	WireCapture AdapterWireCapture `json:"wireCapture,omitzero" toml:"wire_capture,omitempty"`
	// SyntheticContent declares per-provider rules for the synthetic
	// envelope round-trip channel (`<!--clyde-...-->` markers). Each
	// provider bucket controls whether outbound assistant content
	// carries the visible envelope back to Cursor (BYOK has no native
	// thinking UI, so the envelope is the visible affordance) and how
	// inbound assistant content (envelopes Cursor replays back to us
	// on the next turn) is materialized into upstream-native shapes.
	// See [internal/adapter/render/synthetic_content.go] for the marker
	// format and ExtractSyntheticParts contract.
	SyntheticContent AdapterSyntheticContent `json:"syntheticContent,omitzero" toml:"synthetic_content,omitempty"`
	// Retry declares operator-supplied adapter retry policies. This list
	// holds only what config provides; provider packages append their
	// own builtin policies at construction. There is no catch-all retry.
	Retry AdapterRetry `json:"retry,omitzero" toml:"retry,omitempty"`
}

// AdapterRetry is the top-level retry config for adapter operations.
type AdapterRetry struct {
	Policies []AdapterRetryPolicy `json:"policies,omitempty" toml:"policies,omitempty"`
}

// AdapterRetryPolicy is one named retry rule. MaxAttempts is total attempts,
// including the original attempt.
type AdapterRetryPolicy struct {
	Name                     string               `json:"name,omitempty" toml:"name,omitempty"`
	Enabled                  *bool                `json:"enabled,omitempty" toml:"enabled,omitempty"`
	MaxAttempts              int                  `json:"maxAttempts,omitempty" toml:"max_attempts,omitempty"`
	InitialDelay             Duration             `json:"initialDelay,omitempty" toml:"initial_delay,omitempty"`
	MaxDelay                 Duration             `json:"maxDelay,omitempty" toml:"max_delay,omitempty"`
	Multiplier               float64              `json:"multiplier,omitempty" toml:"multiplier,omitempty"`
	JitterFraction           float64              `json:"jitterFraction,omitempty" toml:"jitter_fraction,omitempty"`
	RetryWhenResponseStarted bool                 `json:"retryWhenResponseStarted,omitempty" toml:"retry_when_response_started,omitempty"`
	Match                    AdapterRetryMatchers `json:"match,omitzero" toml:"match,omitempty"`
}

// AdapterRetryMatchers constrains when a retry policy applies.
type AdapterRetryMatchers struct {
	Backends          []string `json:"backends,omitempty" toml:"backends,omitempty"`
	Operations        []string `json:"operations,omitempty" toml:"operations,omitempty"`
	Statuses          []int    `json:"statuses,omitempty" toml:"statuses,omitempty"`
	ErrorClasses      []string `json:"errorClasses,omitempty" toml:"error_classes,omitempty"`
	ErrorCodes        []string `json:"errorCodes,omitempty" toml:"error_codes,omitempty"`
	MessageSubstrings []string `json:"messageSubstrings,omitempty" toml:"message_substrings,omitempty"`
}

// AdapterSyntheticContent holds the per-provider synthetic envelope levers.
// The zero value preserves the documented defaults; provider sub-structs only
// need to be populated when an operator wants to deviate from them.
type AdapterSyntheticContent struct {
	Anthropic           AdapterSyntheticContentProvider `json:"anthropic,omitzero" toml:"anthropic,omitempty"`
	Codex               AdapterSyntheticContentProvider `json:"codex,omitzero" toml:"codex,omitempty"`
	PassthroughOverride AdapterSyntheticContentProvider `json:"passthroughOverride,omitzero" toml:"passthrough_override,omitempty"`
}

// SyntheticInboundMaterialization is the closed enum of inbound thinking
// envelope materialization strategies. Round-tripped thinking content can be
// turned into a native upstream thinking content block, concatenated as
// plain text into the assistant text block, dropped, or passed through
// unchanged with the marker still in place.
type SyntheticInboundMaterialization string

const (
	// SyntheticInboundNativeThinkingBlock materializes round-tripped
	// thinking content as the upstream-native thinking content block
	// (e.g. Anthropic's `{type: "thinking", thinking: ...}`). This is the
	// Anthropic default because Anthropic accepts thinking blocks in the
	// assistant history and the model uses them for reasoning continuity.
	SyntheticInboundNativeThinkingBlock SyntheticInboundMaterialization = "native_thinking_block"
	// SyntheticInboundPlainTextConcat concatenates the envelope body (with
	// decoration stripped) into the assistant text block as plain prose.
	// Useful for upstreams that cannot accept native thinking blocks but
	// where preserving the visible reasoning trace in context still helps.
	SyntheticInboundPlainTextConcat SyntheticInboundMaterialization = "plain_text_concat"
	// SyntheticInboundDrop discards thinking envelope bodies before
	// forwarding upstream. This is the Codex default because Codex
	// upstream cannot accept Anthropic-shaped thinking blocks and the
	// reasoning trace bloats context with no model-side gain.
	SyntheticInboundDrop SyntheticInboundMaterialization = "drop"
	// SyntheticInboundPassthrough leaves the marker-wrapped envelope in
	// place when forwarding upstream. Used by the passthrough override
	// path so the upstream sees exactly what Cursor sent.
	SyntheticInboundPassthrough SyntheticInboundMaterialization = "passthrough"
)

// AdapterSyntheticContentProvider holds the per-provider synthetic envelope
// settings. Pointer-bool for OutboundThinkingEnvelope so we can distinguish
// "operator set false" from "operator unset, take default".
type AdapterSyntheticContentProvider struct {
	// OutboundThinkingEnvelope toggles whether the renderer wraps
	// reasoning bodies in the visible `<!--clyde-thinking-->` envelope
	// on the way to Cursor. Defaults: anthropic=true, codex=true,
	// passthrough_override=false. nil means take the default.
	OutboundThinkingEnvelope *bool `json:"outboundThinkingEnvelope,omitempty" toml:"outbound_thinking_envelope,omitempty"`
	// InboundThinkingMaterialization picks the strategy for
	// round-tripped thinking content on the inbound (request-shaping)
	// side. Empty string means take the default for the provider.
	InboundThinkingMaterialization SyntheticInboundMaterialization `json:"inboundThinkingMaterialization,omitempty" toml:"inbound_thinking_materialization,omitempty"`
}

// AnthropicOutboundThinkingEnvelope returns the resolved per-provider value
// for [AdapterSyntheticContentProvider.OutboundThinkingEnvelope] for the
// Anthropic backend, applying the documented default when unset.
func (s AdapterSyntheticContent) AnthropicOutboundThinkingEnvelope() bool {
	if s.Anthropic.OutboundThinkingEnvelope != nil {
		return *s.Anthropic.OutboundThinkingEnvelope
	}
	return true
}

// AnthropicInboundThinkingMaterialization returns the resolved strategy with
// the documented default when unset.
func (s AdapterSyntheticContent) AnthropicInboundThinkingMaterialization() SyntheticInboundMaterialization {
	if s.Anthropic.InboundThinkingMaterialization != "" {
		return s.Anthropic.InboundThinkingMaterialization
	}
	return SyntheticInboundNativeThinkingBlock
}

// CodexOutboundThinkingEnvelope returns the resolved per-provider value with
// the documented default when unset.
func (s AdapterSyntheticContent) CodexOutboundThinkingEnvelope() bool {
	if s.Codex.OutboundThinkingEnvelope != nil {
		return *s.Codex.OutboundThinkingEnvelope
	}
	return true
}

// CodexInboundThinkingMaterialization returns the resolved strategy with the
// documented default when unset.
func (s AdapterSyntheticContent) CodexInboundThinkingMaterialization() SyntheticInboundMaterialization {
	if s.Codex.InboundThinkingMaterialization != "" {
		return s.Codex.InboundThinkingMaterialization
	}
	return SyntheticInboundDrop
}

// PassthroughOverrideOutboundThinkingEnvelope returns the resolved value with
// the documented default when unset.
func (s AdapterSyntheticContent) PassthroughOverrideOutboundThinkingEnvelope() bool {
	if s.PassthroughOverride.OutboundThinkingEnvelope != nil {
		return *s.PassthroughOverride.OutboundThinkingEnvelope
	}
	return false
}

// PassthroughOverrideInboundThinkingMaterialization returns the resolved
// strategy with the documented default when unset.
func (s AdapterSyntheticContent) PassthroughOverrideInboundThinkingMaterialization() SyntheticInboundMaterialization {
	if s.PassthroughOverride.InboundThinkingMaterialization != "" {
		return s.PassthroughOverride.InboundThinkingMaterialization
	}
	return SyntheticInboundPassthrough
}

// AdapterCodex configures the direct Codex backend path plus optional
// app-server fallback used when direct HTTP fails.
type AdapterCodex struct {
	// Enabled toggles Codex model-name routing.
	Enabled bool `json:"enabled,omitempty" toml:"enabled,omitempty"`
	// BaseURL is the direct backend endpoint.
	// Defaults to https://chatgpt.com/backend-api/codex/responses.
	BaseURL string `json:"baseUrl,omitempty" toml:"base_url,omitempty"`
	// WebsocketEnabled enables the experimental direct websocket
	// transport for the Responses API. Default is false until the
	// parity path is proven; HTTP SSE remains the safe default.
	WebsocketEnabled bool `json:"websocketEnabled,omitempty" toml:"websocket_enabled,omitempty"`
	// AuthFile points at Codex auth state. Defaults to ~/.codex/auth.json.
	AuthFile string `json:"authFile,omitempty" toml:"auth_file,omitempty"`
	// ModelPrefixes are alias prefixes routed to codex when no explicit
	// model entry matches and native_model_routing is "codex".
	// Defaults to ["gpt-", "o"].
	ModelPrefixes []string `json:"modelPrefixes,omitempty" toml:"model_prefixes,omitempty"`
	// NativeModelRouting controls how native OpenAI/Codex-looking model
	// IDs such as gpt-* and o* are handled when they are not declared in
	// [adapter.models]. Empty and "off" reject them as unknown models.
	// "codex" routes through the direct Codex backend.
	// "passthrough_override" routes to NativeModelPassthroughOverride.
	NativeModelRouting string `json:"nativeModelRouting,omitempty" toml:"native_model_routing,omitempty"`
	// NativeModelPassthroughOverride is used when NativeModelRouting is
	// "passthrough_override".
	NativeModelPassthroughOverride string `json:"nativeModelPassthroughOverride,omitempty" toml:"native_model_passthrough_override,omitempty"`
	// ReasoningSummary is the default Codex Responses reasoning.summary
	// value Clyde sends when a reasoning effort is active and the request
	// did not explicitly set reasoning.summary. Valid values match Codex:
	// auto, concise, detailed, none. Empty defaults to auto.
	ReasoningSummary string `json:"reasoningSummary,omitempty" toml:"reasoning_summary,omitempty"`
	// Models declares the Codex-backed model catalog that Clyde
	// advertises and resolves for first-party clyde-* aliases.
	Models []AdapterCodexModel `json:"models,omitempty" toml:"models,omitempty"`
	// WireCapture is the per-provider wire-capture mode block for Codex.
	// Empty mode treats the lever as Off.
	WireCapture AdapterCodexWireCapture `json:"wireCapture,omitzero" toml:"wire_capture,omitempty"`
	// Reasoning carries the per-provider reasoning round-trip levers for
	// the Codex backend. Codex Responses carries TWO independent levers
	// per codex-rs context_manager/history.rs:361-405: visible summary
	// text and encrypted_content memory blob.
	Reasoning AdapterCodexReasoning `json:"reasoning,omitzero" toml:"reasoning,omitempty"`
}

// AdapterWireCapture holds the shared rotation budget that any per-provider
// wire-capture concern uses. Rotation defaults are deliberately small so
// always-on use stays bounded; operators set explicit values when they need
// more retention. The per-provider mode enum lives on each provider's own
// config block to keep legal modes typed per provider.
type AdapterWireCapture struct {
	Rotation LoggingRotation `json:"rotation,omitzero" toml:"rotation,omitempty"`
}

// AdapterAnthropic carries Anthropic-specific sub-blocks introduced after
// the original AdapterConfig shape stabilized.
type AdapterAnthropic struct {
	WireCapture AdapterAnthropicWireCapture `json:"wireCapture,omitzero" toml:"wire_capture,omitempty"`
	// Reasoning carries the per-provider reasoning round-trip levers.
	// Anthropic has a single lever (the visible thinking block); see
	// [AdapterAnthropicReasoning]. Empty values resolve to documented
	// defaults via [AdapterAnthropicReasoning.ResolvedInboundThinking].
	Reasoning AdapterAnthropicReasoning `json:"reasoning,omitzero" toml:"reasoning,omitempty"`
}

// AdapterAnthropicReasoning is the per-provider reasoning lever block for the
// Anthropic backend. Anthropic carries one lever because Anthropic emits a
// single thinking content block per turn; the lever picks how a round-tripped
// thinking envelope is materialized on the inbound (request-shaping) side.
// The legal value set lives in the Anthropic provider package as
// [anthmode.InboundThinking].
type AdapterAnthropicReasoning struct {
	// InboundThinking selects the materialization strategy for round-tripped
	// thinking content. Empty resolves to native_thinking_block via
	// [anthmode.InboundThinking.Resolved].
	InboundThinking anthmode.InboundThinking `json:"inboundThinking,omitempty" toml:"inbound_thinking,omitempty"`
}

// ResolvedInboundThinking returns the configured strategy with the documented
// default applied when unset.
func (r AdapterAnthropicReasoning) ResolvedInboundThinking() anthmode.InboundThinking {
	return r.InboundThinking.Resolved()
}

// AdapterAnthropicWireCapture is the per-provider wire-capture mode block
// for Anthropic. The legal mode set lives in the Anthropic provider package
// as [anthmode.WireCaptureMode]; empty mode is treated as
// [anthmode.WireCaptureOff].
type AdapterAnthropicWireCapture struct {
	Mode anthmode.WireCaptureMode `json:"mode,omitempty" toml:"mode,omitempty"`
}

// CodexWireCaptureMode is the closed enum of legal modes for the Codex
// wire-capture concern. The reasoning_only value is Codex-specific; it
// matches inbound websocket frames where item.type == "reasoning" and is
// the cheapest mode for working on the encrypted_content round-trip.
type CodexWireCaptureMode string

// Codex wire-capture modes.
const (
	// CodexWireCaptureOff disables inbound frame body capture.
	CodexWireCaptureOff CodexWireCaptureMode = "off"
	// CodexWireCaptureSummaryOnly emits a per-frame fingerprint plus
	// the upstream_event_type without the body.
	CodexWireCaptureSummaryOnly CodexWireCaptureMode = "summary_only"
	// CodexWireCaptureReasoningOnly emits the body only on
	// response.output_item.done frames carrying a reasoning item.
	// Cheapest mode for round-trip work.
	CodexWireCaptureReasoningOnly CodexWireCaptureMode = "reasoning_only"
	// CodexWireCaptureFull emits the body on every inbound frame.
	CodexWireCaptureFull CodexWireCaptureMode = "full"
)

// AdapterCodexWireCapture is the per-provider wire-capture mode block for
// Codex. Empty mode is treated as Off.
type AdapterCodexWireCapture struct {
	Mode CodexWireCaptureMode `json:"mode,omitempty" toml:"mode,omitempty"`
}

// CodexRoundTripSummary is the closed enum of strategies for the visible
// reasoning summary text on Codex Responses. Codex carries the summary on
// the same Reasoning item that holds the encrypted_content blob; the two
// levers move independently. Defaults match codex-rs
// research/codex/codex-rs/core/src/context_manager/history.rs:361-405.
type CodexRoundTripSummary string

// Codex round-trip summary strategies.
const (
	// CodexRoundTripSummaryNative materializes round-tripped summary text
	// as the upstream-native summary field on the Reasoning item. This is
	// the documented default.
	CodexRoundTripSummaryNative CodexRoundTripSummary = "native_summary_field"
	// CodexRoundTripSummaryDrop discards summary text before forwarding.
	CodexRoundTripSummaryDrop CodexRoundTripSummary = "drop"
	// CodexRoundTripSummaryPlainText concatenates the envelope body into
	// the assistant text block as plain prose.
	CodexRoundTripSummaryPlainText CodexRoundTripSummary = "plain_text_concat"
)

// CodexRoundTripEncrypted is the closed enum of strategies for the
// encrypted_content reasoning blob on Codex Responses. Empty resolves to
// round_trip per codex-rs.
type CodexRoundTripEncrypted string

// Codex round-trip encrypted_content strategies.
const (
	// CodexRoundTripEncryptedRoundTrip echoes the encrypted_content blob
	// back to Codex on the next turn so the model retains reasoning
	// continuity. Documented default.
	CodexRoundTripEncryptedRoundTrip CodexRoundTripEncrypted = "round_trip"
	// CodexRoundTripEncryptedDrop discards the blob before forwarding.
	CodexRoundTripEncryptedDrop CodexRoundTripEncrypted = "drop"
)

// AdapterCodexReasoning is the per-provider reasoning lever block for the
// Codex backend. Codex carries two levers because Codex Responses can carry
// summary text AND the encrypted_content memory blob on the same Reasoning
// item. Defaults match codex-rs context_manager/history.rs:361-405. The
// encrypted blob is no longer cached on disk; it rides inline on the
// synthetic-thinking close marker so Cursor's transcript owns persistence.
type AdapterCodexReasoning struct {
	// RoundTripSummary selects the strategy for visible summary text.
	// Empty resolves to native_summary_field.
	RoundTripSummary CodexRoundTripSummary `json:"roundTripSummary,omitempty" toml:"round_trip_summary,omitempty"`
	// RoundTripEncrypted selects the strategy for the encrypted_content
	// blob. Empty resolves to round_trip.
	RoundTripEncrypted CodexRoundTripEncrypted `json:"roundTripEncrypted,omitempty" toml:"round_trip_encrypted,omitempty"`
}

// ResolvedRoundTripSummary returns the configured strategy with the
// documented default applied when unset.
func (r AdapterCodexReasoning) ResolvedRoundTripSummary() CodexRoundTripSummary {
	if r.RoundTripSummary == "" {
		return CodexRoundTripSummaryNative
	}
	return r.RoundTripSummary
}

// ResolvedRoundTripEncrypted returns the configured strategy with the
// documented default applied when unset.
func (r AdapterCodexReasoning) ResolvedRoundTripEncrypted() CodexRoundTripEncrypted {
	if r.RoundTripEncrypted == "" {
		return CodexRoundTripEncryptedRoundTrip
	}
	return r.RoundTripEncrypted
}

// ResolvedAnthropicWireCaptureMode returns the configured mode with the Off
// default applied when the operator has not set a value.
func (c AdapterAnthropic) ResolvedAnthropicWireCaptureMode() anthmode.WireCaptureMode {
	return c.WireCapture.Mode.Resolved()
}

// ResolvedCodexWireCaptureMode returns the configured mode with the Off
// default applied when the operator has not set a value.
func (c AdapterCodex) ResolvedCodexWireCaptureMode() CodexWireCaptureMode {
	if c.WireCapture.Mode == "" {
		return CodexWireCaptureOff
	}
	return c.WireCapture.Mode
}

type AdapterCodexModel struct {
	AliasPrefix      string                     `json:"aliasPrefix,omitempty" toml:"alias_prefix,omitempty"`
	Model            string                     `json:"model,omitempty" toml:"model,omitempty"`
	InstructionsFile string                     `json:"instructionsFile,omitempty" toml:"instructions_file,omitempty"`
	Instructions     string                     `json:"-" toml:"-"`
	Efforts          []string                   `json:"efforts,omitempty" toml:"efforts,omitempty"`
	MaxOutputTokens  int                        `json:"maxOutputTokens,omitempty" toml:"max_output_tokens,omitempty"`
	Contexts         []AdapterCodexModelContext `json:"contexts,omitempty" toml:"contexts,omitempty"`
}

type AdapterCodexModelContext struct {
	Tokens int `json:"tokens,omitempty" toml:"tokens,omitempty"`
	// ObservedTokens is the context window the Codex Responses HTTP
	// transport has actually accepted for this advertised variant.
	// When zero, Clyde treats the observed window as equal to Tokens.
	ObservedTokens int    `json:"observedTokens,omitempty" toml:"observed_tokens,omitempty"`
	AliasSuffix    string `json:"aliasSuffix,omitempty" toml:"alias_suffix,omitempty"`
	// NativeAliases are OpenAI/Codex-looking ids that should resolve to
	// this context when [adapter.codex].native_model_routing is "codex".
	NativeAliases []string `json:"nativeAliases,omitempty" toml:"native_aliases,omitempty"`
	// AdvertisedNativeAliases is the subset of native aliases Clyde should
	// include in /v1/models. Aliases listed here also resolve natively.
	AdvertisedNativeAliases []string `json:"advertisedNativeAliases,omitempty" toml:"advertised_native_aliases,omitempty"`
}

// AdapterLogprobs picks the per-backend behavior. Each value is
// one of "reject" (return 400 when caller sets logprobs) or
// "drop" (silently strip the field before forwarding). OpenAI-compatible
// passthrough routes pass through verbatim regardless of this stanza.
type AdapterLogprobs struct {
	Anthropic string `json:"anthropic,omitempty" toml:"anthropic,omitempty"`
}

// AdapterOpenAICompatPassthrough points unknown model aliases at one
// OpenAI-compatible upstream. The caller's model string is preserved unless
// Model is set.
type AdapterOpenAICompatPassthrough struct {
	BaseURL string `json:"baseUrl,omitempty" toml:"base_url,omitempty"`
	APIKey  string `json:"apiKey,omitempty" toml:"api_key,omitempty"`
	// APIKeyEnv lets the user keep the secret out of the config file.
	// When set the adapter reads os.Getenv(APIKeyEnv) at request time.
	APIKeyEnv string `json:"apiKeyEnv,omitempty" toml:"api_key_env,omitempty"`
	// Model overrides the model name forwarded upstream. Empty means pass
	// the caller's model string through unchanged.
	Model string `json:"model,omitempty" toml:"model,omitempty"`
}

// AdapterNotices controls the synthetic notice injection path across
// direct provider responses. Omitted defaults to enabled=true with the
// built-in usage warning thresholds.
type AdapterNotices struct {
	Enabled *bool              `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Usage   AdapterNoticeUsage `json:"usage,omitzero" toml:"usage,omitempty"`
}

// AdapterNoticeUsage configures when usage-related notices should be
// injected into assistant responses. Threshold values represent used
// percent, not remaining percent. For example, 75 means inject once the
// provider reports 75%% used or higher.
type AdapterNoticeUsage struct {
	ThresholdsUsedPercent []float64                 `json:"thresholdsUsedPercent,omitempty" toml:"thresholds_used_percent,omitempty"`
	Repeat                AdapterNoticeRepeatPolicy `json:"repeat,omitzero" toml:"repeat,omitempty"`
}

type AdapterNoticeRepeatMode string

const (
	AdapterNoticeRepeatEvery                      AdapterNoticeRepeatMode = "every"
	AdapterNoticeRepeatOncePerThresholdUntilReset AdapterNoticeRepeatMode = "once_per_threshold_until_reset"
	AdapterNoticeRepeatTimeCooldown               AdapterNoticeRepeatMode = "time_cooldown"
	AdapterNoticeRepeatTurnCooldown               AdapterNoticeRepeatMode = "turn_cooldown"
)

type AdapterNoticeRepeatPolicy struct {
	Mode             AdapterNoticeRepeatMode `json:"mode,omitempty" toml:"mode,omitempty"`
	Cooldown         string                  `json:"cooldown,omitempty" toml:"cooldown,omitempty"`
	CooldownDuration time.Duration           `json:"-" toml:"-"`
	CooldownTurns    int                     `json:"cooldownTurns,omitempty" toml:"cooldown_turns,omitempty"`
}

var defaultNoticeUsageThresholdsUsedPercent = []float64{75, 95}

// EnabledOrDefault returns true when the stanza is absent or enabled is unset.
func (n AdapterNotices) EnabledOrDefault() bool {
	if n.Enabled == nil {
		return true
	}
	return *n.Enabled
}

// UsageThresholdsUsedPercentOrDefault returns the normalized used-percent
// thresholds for chat usage notices.
func (n AdapterNotices) UsageThresholdsUsedPercentOrDefault() []float64 {
	if len(n.Usage.ThresholdsUsedPercent) == 0 {
		return append([]float64(nil), defaultNoticeUsageThresholdsUsedPercent...)
	}
	return append([]float64(nil), n.Usage.ThresholdsUsedPercent...)
}

func (n AdapterNotices) UsageRepeatPolicyOrDefault() AdapterNoticeRepeatPolicy {
	policy := n.Usage.Repeat
	if policy.Mode == "" {
		policy.Mode = AdapterNoticeRepeatEvery
	}
	return policy
}

// AdapterOAuth holds endpoints and OAuth client metadata supplied by
// the operator. No defaults are compiled into clyde.
type AdapterOAuth struct {
	TokenURL         string   `json:"tokenUrl,omitempty" toml:"token_url,omitempty"`
	MessagesURL      string   `json:"messagesUrl,omitempty" toml:"messages_url,omitempty"`
	ClientID         string   `json:"clientId,omitempty" toml:"client_id,omitempty"`
	AnthropicBeta    string   `json:"anthropicBeta,omitempty" toml:"anthropic_beta,omitempty"`
	AnthropicVersion string   `json:"anthropicVersion,omitempty" toml:"anthropic_version,omitempty"`
	KeychainService  string   `json:"keychainService,omitempty" toml:"keychain_service,omitempty"`
	Scopes           []string `json:"scopes,omitempty" toml:"scopes,omitempty"`
	// ToolResultCacheReferenceEnabled controls whether Clyde emits
	// tool_result.cache_reference on the direct Anthropic OAuth path.
	// Default is false because the live Anthropic /v1/messages OAuth
	// tool-followup path rejected this field in production and MITM
	// captures of the official Claude CLI succeeded without it.
	ToolResultCacheReferenceEnabled bool `json:"toolResultCacheReferenceEnabled,omitempty" toml:"tool_result_cache_reference_enabled,omitempty"`
}

// ValidateOAuthFields returns an error if any required field is empty.
func (o AdapterOAuth) ValidateOAuthFields() error {
	if o.TokenURL == "" {
		return fmt.Errorf("adapter: [adapter.oauth].token_url must be set")
	}
	if o.MessagesURL == "" {
		return fmt.Errorf("adapter: [adapter.oauth].messages_url must be set")
	}
	if o.ClientID == "" {
		return fmt.Errorf("adapter: [adapter.oauth].client_id must be set")
	}
	if o.AnthropicBeta == "" {
		return fmt.Errorf("adapter: [adapter.oauth].anthropic_beta must be set")
	}
	if o.AnthropicVersion == "" {
		return fmt.Errorf("adapter: [adapter.oauth].anthropic_version must be set")
	}
	if o.KeychainService == "" {
		return fmt.Errorf("adapter: [adapter.oauth].keychain_service must be set")
	}
	if len(o.Scopes) == 0 {
		return fmt.Errorf("adapter: [adapter.oauth].scopes must be non-empty")
	}
	return nil
}

// AdapterClientIdentity holds header and body-side wire fields for
// direct HTTP chat. All listed fields are required at registry
// construction unless noted.
type AdapterClientIdentity struct {
	BetaHeader              string `json:"betaHeader,omitempty" toml:"beta_header,omitempty"`
	UserAgent               string `json:"userAgent,omitempty" toml:"user_agent,omitempty"`
	SystemPromptPrefix      string `json:"systemPromptPrefix,omitempty" toml:"system_prompt_prefix,omitempty"`
	StainlessPackageVersion string `json:"stainlessPackageVersion,omitempty" toml:"stainless_package_version,omitempty"`
	StainlessRuntime        string `json:"stainlessRuntime,omitempty" toml:"stainless_runtime,omitempty"`
	StainlessRuntimeVersion string `json:"stainlessRuntimeVersion,omitempty" toml:"stainless_runtime_version,omitempty"`
	CCVersion               string `json:"ccVersion,omitempty" toml:"cc_version,omitempty"`
	CCEntrypoint            string `json:"ccEntrypoint,omitempty" toml:"cc_entrypoint,omitempty"`
	// PerContextBetas maps a substring of the wire model id (e.g. a
	// context suffix) to an extra anthropic-beta flag for that variant.
	PerContextBetas map[string]string `json:"perContextBetas,omitempty" toml:"per_context_betas,omitempty"`
	// PromptCachingEnabled toggles the typed-system-blocks form with
	// cache_control markers on the billing / CLI-prefix / caller-system
	// blocks. When nil or true, markers are stamped and system is sent
	// as a typed block array. When false, system is sent as a plain
	// string (back-compat wire shape). Safety valve if the upstream
	// identity check ever disagrees with the marker form.
	PromptCachingEnabled *bool `json:"promptCachingEnabled,omitempty" toml:"prompt_caching_enabled,omitempty"`
	// PromptCacheTTL selects the cache breakpoint TTL. Empty (default)
	// uses Anthropic's 5m default (writes cost 1.25x input). "1h"
	// extends the TTL at a write cost of 2x input; only worthwhile for
	// long-idle reuse (user pauses 5m+ between turns). Reads are 0.1x
	// input at either TTL. Anything other than "" / "5m" / "1h" is
	// ignored and treated as default.
	PromptCacheTTL string `json:"promptCacheTTL,omitempty" toml:"prompt_cache_ttl,omitempty"`
	// PromptCacheScope selects the cache_control scope on the CLI
	// system prefix block. Empty (default) uses session-scoped
	// caching, same as today. "global" asks Anthropic for a shared
	// cache key across sessions; only effective on accounts Anthropic
	// allowlists. "org" scopes to the billing org. Anything else is
	// ignored. Requires the prompt-caching-scope-2026-01-05 beta
	// header in [adapter.client_identity.beta_header] to be effective.
	PromptCacheScope string `json:"promptCacheScope,omitempty" toml:"prompt_cache_scope,omitempty"`
	// MicrocompactEnabled rewrites aged tool_result bodies to a
	// placeholder string before sending, mirroring Claude Code's
	// time-based microcompact. Defaults to true when nil. Set to false
	// if upstream caching is misbehaving and we need to isolate.
	MicrocompactEnabled *bool `json:"microcompactEnabled,omitempty" toml:"microcompact_enabled,omitempty"`
	// MicrocompactKeepRecent is how many most-recent compactable tool
	// results are kept verbatim. Older ones get cleared. Defaults to
	// 15 when nil or zero. Match Claude's GrowthBook default when it
	// diverges.
	MicrocompactKeepRecent int `json:"microcompactKeepRecent,omitempty" toml:"microcompact_keep_recent,omitempty"`
}

// AdapterFamily describes one Claude model family and the cross
// product of efforts, thinking state, and context windows the
// registry expands into individual aliases. The registry generator
// produces aliases of shape
// `clyde-<alias_prefix>-<ctx>-<effort>[-thinking]`.
type AdapterFamily struct {
	// AliasPrefix is the public clyde-* model stem without the
	// leading "clyde-". When empty, the family map key is used.
	AliasPrefix string `json:"aliasPrefix,omitempty" toml:"alias_prefix,omitempty"`
	// Model is the wire-level model id (e.g. a snapshot name). The
	// Contexts entries may add a wire
	// suffix (e.g. "[1m]") when calling /v1/messages.
	Model string `json:"model,omitempty" toml:"model,omitempty"`
	// InstructionsFile points at a markdown file whose verbatim contents
	// are loaded once during config parsing and copied onto expanded
	// registry aliases. Relative paths resolve from the declaring
	// config.toml directory.
	InstructionsFile string `json:"instructionsFile,omitempty" toml:"instructions_file,omitempty"`
	// Instructions carries the loaded file contents for registry
	// construction. It is not serialized back to disk.
	Instructions string `json:"-" toml:"-"`
	// Efforts enumerates effort tiers the wire API accepts for this
	// family. Empty means the server rejects effort on this family
	// (the registry will refuse caller-supplied effort with 400).
	Efforts []string `json:"efforts,omitempty" toml:"efforts,omitempty"`
	// ThinkingModes enumerates the thinking modes the wire API
	// accepts. Always at least default+enabled+disabled for
	// thinking-capable families; adaptive is gated server-side.
	ThinkingModes []string `json:"thinkingModes,omitempty" toml:"thinking_modes,omitempty"`
	// ThinkingWireMode controls the upstream thinking shape for
	// aliases generated with thinking enabled. Valid values are
	// "enabled" (default; sends a typed thinking block with
	// budget_tokens) and "adaptive" (sends Anthropic's adaptive
	// variant, no budget). Some families require adaptive at the
	// upstream (claude-opus-4-7 historically rejected enabled). Set
	// "adaptive" to override per family. Empty means "enabled".
	ThinkingWireMode string `json:"thinkingWireMode,omitempty" toml:"thinking_wire_mode,omitempty"`
	// MaxOutputTokens caps this family's output. Used to derive
	// thinking.budget_tokens (budget = max - 1) per the CLI's
	// invariant.
	MaxOutputTokens int `json:"maxOutputTokens,omitempty" toml:"max_output_tokens,omitempty"`
	// SupportsTools declares whether this family accepts the
	// Anthropic tools/tool_choice request fields. There is no
	// default: NewRegistry rejects a family with the field unset
	// (nil pointer means "user did not say"). Set true for opus,
	// sonnet, haiku-4-5; set false for legacy text-only snapshots.
	SupportsTools *bool `json:"supportsTools,omitempty" toml:"supports_tools,omitempty"`
	// SupportsVision declares whether this family accepts image
	// content blocks on user messages. Same fail-loud contract as
	// SupportsTools.
	SupportsVision *bool `json:"supportsVision,omitempty" toml:"supports_vision,omitempty"`
	// Contexts pairs an advertised context window (tokens) with an
	// alias suffix and a wire suffix. At least one entry required.
	Contexts []AdapterModelContext `json:"contexts,omitempty" toml:"contexts,omitempty"`
}

// AdapterModelContext binds one context-window variant for a family.
// The alias suffix is appended to the public alias; the wire suffix
// is appended to the model id sent to /v1/messages (e.g. "[1m]" for
// the 1M-context Opus snapshot).
type AdapterModelContext struct {
	Tokens int `json:"tokens,omitempty" toml:"tokens,omitempty"`
	// ObservedTokens is the context window Clyde should surface for
	// capability reports when it differs from the nominal Tokens
	// value. Mirrors the Codex family `observed_tokens` semantics so
	// other OpenAI-SDK clients see a truthful capacity. Zero falls
	// back to Tokens.
	ObservedTokens int    `json:"observedTokens,omitempty" toml:"observed_tokens,omitempty"`
	AliasSuffix    string `json:"aliasSuffix,omitempty" toml:"alias_suffix,omitempty"`
	WireSuffix     string `json:"wireSuffix,omitempty" toml:"wire_suffix,omitempty"`
}

// AdapterModel describes one backend the adapter can route to.
// Backend is either "claude" or "passthrough_override". For claude
// backends, Model names the real Claude model passed through via
// --model. Context sets the advertised context window. Efforts names
// the allowed reasoning effort tiers for this model. The first entry
// is the default when the request does not specify one.
type AdapterModel struct {
	Backend string `json:"backend,omitempty" toml:"backend,omitempty"`
	Model   string `json:"model,omitempty" toml:"model,omitempty"`
	// InstructionsFile points at a markdown file whose verbatim contents
	// are loaded once during config parsing. Relative paths resolve from
	// the declaring config.toml directory.
	InstructionsFile string `json:"instructionsFile,omitempty" toml:"instructions_file,omitempty"`
	// Instructions carries the loaded file contents for registry
	// construction. It is not serialized back to disk.
	Instructions string `json:"-" toml:"-"`
	Context      int    `json:"context,omitempty" toml:"context,omitempty"`
	// ObservedContext is the provider-specific context window Clyde
	// should surface for capability reports when it differs from the
	// advertised context. Zero means use Context.
	ObservedContext int      `json:"observedContext,omitempty" toml:"observed_context,omitempty"`
	Efforts         []string `json:"efforts,omitempty" toml:"efforts,omitempty"`
	// PassthroughOverride names an entry in
	// AdapterConfig.PassthroughOverrides for backend
	// "passthrough_override".
	PassthroughOverride string `json:"passthroughOverride,omitempty" toml:"passthrough_override,omitempty"`
}

// AdapterPassthroughOverride points to an upstream OpenAI-compatible endpoint.
type AdapterPassthroughOverride struct {
	BaseURL string `json:"baseUrl,omitempty" toml:"base_url,omitempty"`
	APIKey  string `json:"apiKey,omitempty" toml:"api_key,omitempty"`
	// APIKeyEnv lets the user keep the secret out of the config
	// file. When set the adapter reads os.Getenv(APIKeyEnv) at
	// request time.
	APIKeyEnv string `json:"apiKeyEnv,omitempty" toml:"api_key_env,omitempty"`
	// Model overrides the model name forwarded upstream. Empty
	// means pass the caller's model string through unchanged.
	Model string `json:"model,omitempty" toml:"model,omitempty"`
}

// SearchConfig configures the LLM backend for conversation search.
type SearchConfig struct {
	// Backend is "claude" (default) or "local"
	Backend string       `json:"backend,omitempty" toml:"backend,omitempty"`
	Local   SearchLocal  `json:"local,omitzero" toml:"local,omitempty"`
	Claude  SearchClaude `json:"claude,omitzero" toml:"claude,omitempty"`
}

// SearchLocal configures a local OpenAI-compatible LLM endpoint.
type SearchLocal struct {
	URL                string        `json:"url,omitempty" toml:"url,omitempty"`
	Token              string        `json:"token,omitempty" toml:"token,omitempty"`
	EmbeddingURL       string        `json:"embeddingUrl,omitempty" toml:"embedding_url,omitempty"`
	EmbeddingToken     string        `json:"embeddingToken,omitempty" toml:"embedding_token,omitempty"`
	Model              string        `json:"model,omitempty" toml:"model,omitempty"`
	RerankModel        string        `json:"rerankModel,omitempty" toml:"rerank_model,omitempty"`
	DeepModel          string        `json:"deepModel,omitempty" toml:"deep_model,omitempty"`
	Pipeline           []SearchLayer `json:"pipeline,omitempty" toml:"pipeline,omitempty"`
	Temperature        float64       `json:"temperature" toml:"temperature"`
	TopP               float64       `json:"topP" toml:"top_p"`
	FrequencyPenalty   float64       `json:"frequencyPenalty" toml:"frequency_penalty"`
	MaxConcurrent      int           `json:"maxConcurrent,omitempty" toml:"max_concurrent,omitempty"`
	ChunkSize          int           `json:"chunkSize,omitempty" toml:"chunk_size,omitempty"`
	MaxMemoryGB        int           `json:"maxMemoryGB,omitempty" toml:"max_memory_gb,omitempty"`
	ContextLength      int           `json:"contextLength,omitempty" toml:"context_length,omitempty"`
	EmbeddingThreshold float64       `json:"embeddingThreshold,omitempty" toml:"embedding_threshold,omitempty"`
	EmbeddingModel     string        `json:"embeddingModel,omitempty" toml:"embedding_model,omitempty"`
}

// ResolvedEmbeddingURL returns the base URL for OpenAI-style embedding
// requests (scheme plus host plus port, no trailing slash, no /v1 suffix).
// When EmbeddingURL is empty it falls back to URL.
func (s SearchLocal) ResolvedEmbeddingURL() string {
	if trimmed := strings.TrimSpace(s.EmbeddingURL); trimmed != "" {
		return strings.TrimSuffix(trimmed, "/")
	}
	return strings.TrimSuffix(strings.TrimSpace(s.URL), "/")
}

// ResolvedEmbeddingToken returns the bearer token for the embedding
// endpoint. When EmbeddingToken is empty it falls back to Token.
func (s SearchLocal) ResolvedEmbeddingToken() string {
	if s.EmbeddingToken != "" {
		return s.EmbeddingToken
	}
	return s.Token
}

// SearchLayer defines one stage of the search pipeline.
type SearchLayer struct {
	Name  string `json:"name" toml:"name"`   // "sweep", "rerank", "deep"
	Model string `json:"model" toml:"model"` // model to use for this layer
}

// ResolvePipeline returns the LLM pipeline layers for a given depth.
//
// Depth levels:
//   - "quick"      -- embedding similarity only, no LLM (returns nil)
//   - "normal"     -- embedding filter + LLM sweep (1 layer)
//   - "deep"       -- embedding filter + sweep + rerank (2 layers)
//   - "extra-deep" -- full pipeline including deep analysis (all layers)
func (s SearchLocal) ResolvePipeline(depth string) []SearchLayer {
	// "quick" skips LLM entirely, handled by the embedding-only path in searchInternal.
	if depth == "quick" {
		return nil
	}

	// If explicit pipeline is configured, slice it to the requested depth.
	if len(s.Pipeline) > 0 {
		switch depth {
		case "normal":
			if len(s.Pipeline) >= 1 {
				return s.Pipeline[:1]
			}
		case "deep":
			if len(s.Pipeline) >= 2 {
				return s.Pipeline[:2]
			}
			return s.Pipeline
		default: // "extra-deep" and anything else: full pipeline
			return s.Pipeline
		}
		return s.Pipeline
	}

	// Fall back to individual model fields.
	var layers []SearchLayer
	model := s.Model
	if model == "" {
		model = "qwen2.5-coder-32b"
	}
	layers = append(layers, SearchLayer{Name: "sweep", Model: model})

	if depth == "normal" {
		return layers
	}

	if s.RerankModel != "" {
		layers = append(layers, SearchLayer{Name: "rerank", Model: s.RerankModel})
	}

	if depth == "extra-deep" && s.DeepModel != "" {
		layers = append(layers, SearchLayer{Name: "deep", Model: s.DeepModel})
	}

	return layers
}

// SearchClaude configures the Claude backend for search.
type SearchClaude struct {
	Model string `json:"model,omitempty" toml:"model,omitempty"`
}

// Defaults are session defaults applied to all sessions.
type Defaults struct {
	RemoteControl   bool   `json:"remoteControl,omitempty" toml:"remote_control,omitempty"`
	Model           string `json:"model,omitempty" toml:"model,omitempty"`
	EffortLevel     string `json:"effortLevel,omitempty" toml:"effort_level,omitempty"`
	AnthropicAPIKey string `json:"anthropicApiKey,omitempty" toml:"anthropic_api_key,omitempty"`
	CompactCounter  string `json:"compactCounter,omitempty" toml:"compact_counter,omitempty"`
}

// AutoNameConfig holds the [autoname] block of clyde.toml.
//
// Enabled is the global kill switch. The system is on by default.
// Operators set this to false to disable the auto-rename worker.
//
// Provider is the adapter route key the worker uses for the LLM
// candidate-name call. The empty value means "fall back to whatever
// the summary subsystem uses today" so day-one behavior matches the
// model the operator already trusts. The worker resolves the route at
// call time. Do not hardcode a model name here.
//
// MaxCallsPerHour caps the daemon-wide LLM call rate. Default 6.
//
// Cooldown is the minimum interval between probe attempts on the
// same session. Default 30 minutes.
//
// MinUserMessages is the trigger threshold. Default 3 user messages
// before a cold session enters the auto-rename pipeline.
//
// Redact controls the redaction pass on transcript content before it
// reaches the LLM call.
type AutoNameConfig struct {
	Enabled         *bool        `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Provider        string       `json:"provider,omitempty" toml:"provider,omitempty"`
	MaxCallsPerHour int          `json:"maxCallsPerHour,omitempty" toml:"max_calls_per_hour,omitempty"`
	Cooldown        Duration     `json:"cooldown,omitempty" toml:"cooldown,omitempty"`
	MinUserMessages int          `json:"minUserMessages,omitempty" toml:"min_user_messages,omitempty"`
	Redact          RedactPolicy `json:"redact,omitzero" toml:"redact,omitempty"`
}

// IsEnabled reports whether the auto-rename worker is enabled.
// Treats a nil Enabled pointer as the default (true).
func (a AutoNameConfig) IsEnabled() bool {
	if a.Enabled == nil {
		return true
	}
	return *a.Enabled
}

// RedactPolicy controls the auto-rename redaction pass.
//
// MinDigitRunForRedact is the minimum length of consecutive digits to
// redact (e.g. 7 to drop phone numbers but keep small ints).
// Default 7.
//
// StripEmails strips email-shaped substrings. Default true.
// StripPaths strips substrings starting with `/`. Default true.
// StripKeyPrefixes strips obvious credential prefixes (sk-, ghp_,
// AKIA, AIza, etc.). Default true.
//
// All three Strip* flags use *bool so a partial [autoname.redact]
// block can opt one off without flipping the others off by zero
// value.
type RedactPolicy struct {
	MinDigitRunForRedact int   `json:"minDigitRunForRedact,omitempty" toml:"min_digit_run_for_redact,omitempty"`
	StripEmails          *bool `json:"stripEmails,omitempty" toml:"strip_emails,omitempty"`
	StripPaths           *bool `json:"stripPaths,omitempty" toml:"strip_paths,omitempty"`
	StripKeyPrefixes     *bool `json:"stripKeyPrefixes,omitempty" toml:"strip_key_prefixes,omitempty"`
}

// StripEmailsOrDefault returns true when StripEmails is unset.
func (r RedactPolicy) StripEmailsOrDefault() bool {
	if r.StripEmails == nil {
		return true
	}
	return *r.StripEmails
}

// StripPathsOrDefault returns true when StripPaths is unset.
func (r RedactPolicy) StripPathsOrDefault() bool {
	if r.StripPaths == nil {
		return true
	}
	return *r.StripPaths
}

// StripKeyPrefixesOrDefault returns true when StripKeyPrefixes is unset.
func (r RedactPolicy) StripKeyPrefixesOrDefault() bool {
	if r.StripKeyPrefixes == nil {
		return true
	}
	return *r.StripKeyPrefixes
}

// MITMConfig configures the local capture proxy and its persistence.
// RawCaptureEnabled is derived from logging.raw_capture.enabled during config load
// and is not a user-facing MITM config key.
type MITMConfig struct {
	EnabledDefault    bool                   `json:"enabledDefault,omitempty" toml:"enabled_default,omitempty"`
	Providers         MITMProviderSet        `json:"providers,omitempty" toml:"providers,omitempty"`
	RawCaptureEnabled bool                   `json:"-" toml:"-"`
	CaptureDir        string                 `json:"captureDir,omitempty" toml:"capture_dir,omitempty"`
	Capture           MITMCapture            `json:"capture,omitzero" toml:"capture,omitempty"`
	CaptureRules      []MITMCaptureRouteRule `json:"captureRules,omitempty" toml:"capture_rules,omitempty"`
	Hooks             []MITMHookRule         `json:"hooks,omitempty" toml:"hook,omitempty"`
	Drift             MITMDriftConfig        `json:"drift,omitzero" toml:"drift,omitempty"`
	Listen            MITMListenConfig       `json:"listen,omitzero" toml:"listen,omitempty"`
	CA                MITMCAConfig           `json:"ca,omitzero" toml:"ca,omitempty"`
}

// MITMListenConfig configures the stable listen address of the in-process
// MITM proxy. Host and Port are config-driven so reloads keep the URL stable
// across daemon restarts; clients pin this address and would break if the
// daemon picked an ephemeral port on every reload.
type MITMListenConfig struct {
	Host string `json:"host,omitempty" toml:"host,omitempty"`
	Port int    `json:"port,omitempty" toml:"port,omitempty"`
}

// MITMCAConfig configures on-disk persistence of the MITM CA. The certificate
// and key are written to these absolute paths so the CA survives daemon
// restarts and clients can install the cert once and trust it across reloads.
type MITMCAConfig struct {
	CertPath string `json:"certPath,omitempty" toml:"cert_path,omitempty"`
	KeyPath  string `json:"keyPath,omitempty"  toml:"key_path,omitempty"`
}

// MITMCapture configures MITM capture index file policy. Raw request and
// response body captures are separate files under CaptureDir/raw and are kept
// out of this rotation policy.
type MITMCapture struct {
	Rotation LoggingRotation `json:"rotation,omitzero" toml:"rotation,omitempty"`
}

// MITMProviderSet is the configured set of provider families routed through
// the capture proxy. The special value "all" enables every provider family.
type MITMProviderSet []string

// MITMCaptureRouteRule classifies one captured MITM request into a concern.
// Rules are evaluated in order, and every non-empty predicate must match.
type MITMCaptureRouteRule struct {
	Concern             string `json:"concern" toml:"concern"`
	Provider            string `json:"provider,omitempty" toml:"provider,omitempty"`
	Host                string `json:"host,omitempty" toml:"host,omitempty"`
	Method              string `json:"method,omitempty" toml:"method,omitempty"`
	PathExact           string `json:"pathExact,omitempty" toml:"path_exact,omitempty"`
	PathPrefix          string `json:"pathPrefix,omitempty" toml:"path_prefix,omitempty"`
	PathContains        string `json:"pathContains,omitempty" toml:"path_contains,omitempty"`
	ContentTypeContains string `json:"contentTypeContains,omitempty" toml:"content_type_contains,omitempty"`
}

// MITMHookMode names one of the three supported hook execution modes
// for [[mitm.hook]] rules. The mode controls whether clyde contacts
// upstream before or after the hook runs, and which body the hook
// receives on stdin.
type MITMHookMode string

const (
	// MITMHookModeSynthesize skips the upstream call entirely. The hook
	// produces the response body from the request alone. Use this when
	// the proxy should fabricate a reply (stub, error injection,
	// always-no-update response).
	MITMHookModeSynthesize MITMHookMode = "synthesize"
	// MITMHookModeTransformRequest invokes the hook before the upstream
	// call so the hook can rewrite the outbound request body. clyde
	// then forwards the rewritten request to upstream and streams
	// upstream's response back unchanged.
	MITMHookModeTransformRequest MITMHookMode = "transform_request"
	// MITMHookModeTransformResponse forwards the request to upstream
	// first, then invokes the hook with upstream's response body so the
	// hook can rewrite it before clyde streams the result back to the
	// client. This is the mode the desktop-via-clyde Cursor update
	// re-patching flow uses.
	MITMHookModeTransformResponse MITMHookMode = "transform_response"
)

// IsValid reports whether the mode is one of the three known values.
func (m MITMHookMode) IsValid() bool {
	switch m {
	case MITMHookModeSynthesize, MITMHookModeTransformRequest, MITMHookModeTransformResponse:
		return true
	}
	return false
}

// MITMHookRule declares an external subprocess that clyde forks for
// every MITM-decrypted Cursor request whose host and path satisfy the
// matchers. Hooks let external tools (such as desktop-via-clyde)
// rewrite or synthesize traffic without coupling clyde to the
// app-specific logic. The hook receives one JSON envelope on stdin
// describing temp-file paths for the bodies and writes one JSON
// envelope on stdout describing the response. See
// internal/mitm/hook.go for the envelope contract.
type MITMHookRule struct {
	// Name is an operator-facing identifier echoed in clyde logs and
	// in the capture event's hook field. Required.
	Name string `json:"name" toml:"name"`
	// MatchHost is a literal hostname matched against the intercepted
	// request's Host header. A single leading "*." enables suffix
	// matching ("*.cursor.com"). Empty matches every host.
	MatchHost string `json:"matchHost,omitempty" toml:"match_host,omitempty"`
	// MatchPathRegex is a Go regexp evaluated against the intercepted
	// request's URL path. Empty matches every path.
	MatchPathRegex string `json:"matchPathRegex,omitempty" toml:"match_path_regex,omitempty"`
	// MatchMethod restricts the rule to one HTTP method when set.
	MatchMethod string `json:"matchMethod,omitempty" toml:"match_method,omitempty"`
	// Mode selects which of the three execution shapes the dispatcher
	// uses. Defaults to MITMHookModeTransformResponse when empty.
	Mode MITMHookMode `json:"mode,omitempty" toml:"mode,omitempty"`
	// Command is the absolute path to the hook binary that clyde
	// forks. Required.
	Command string `json:"command" toml:"command"`
	// Args are extra argv entries passed before the per-request
	// arguments clyde appends.
	Args []string `json:"args,omitempty" toml:"args,omitempty"`
	// Timeout caps how long clyde waits for the hook subprocess to
	// finish before killing it and returning 502 to the client. Empty
	// uses the dispatcher's default (5 minutes).
	Timeout Duration `json:"timeout,omitempty" toml:"timeout,omitempty"`
}

// Duration is the shared [time.Duration] wrapper for TOML-decoded
// config fields. The TOML decoder cannot read a bare [time.Duration],
// so every duration-shaped config field aliases this type and gets
// `"10m"`-style strings parsed via UnmarshalText.
type Duration time.Duration

// UnmarshalText parses a quoted Go duration string or an integer
// nanosecond count.
func (duration *Duration) UnmarshalText(text []byte) error {
	value := strings.TrimSpace(string(text))
	if value == "" {
		*duration = 0
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err == nil {
		*duration = Duration(parsed)
		return nil
	}
	numeric, numericErr := strconv.ParseInt(value, 10, 64)
	if numericErr == nil {
		*duration = Duration(time.Duration(numeric))
		return nil
	}
	return fmt.Errorf("parse duration %q: %w", value, err)
}

// AsDuration returns the standard library duration value.
func (duration *Duration) AsDuration() time.Duration {
	return time.Duration(*duration)
}

// MITMDriftConfig configures daemon-owned baseline refresh and drift
// reporting. When Enabled, the daemon periodically reads the current
// local capture store, refreshes each upstream baseline, and appends
// drift outcomes to per-upstream JSONL logs under DriftLogDir before
// accepting a changed baseline.
type MITMDriftConfig struct {
	Enabled     bool                            `json:"enabled,omitempty" toml:"enabled,omitempty"`
	Interval    time.Duration                   `json:"interval,omitempty" toml:"interval,omitempty"`
	DriftLogDir string                          `json:"driftLogDir,omitempty" toml:"drift_log_dir,omitempty"`
	CaptureRoot string                          `json:"captureRoot,omitempty" toml:"capture_root,omitempty"`
	CACertPath  string                          `json:"caCertPath,omitempty" toml:"ca_cert_path,omitempty"`
	Upstreams   map[string]MITMDriftUpstreamCfg `json:"upstreams,omitempty" toml:"upstreams,omitempty"`
}

// MITMDriftUpstreamCfg configures one upstream's daemon-owned baseline
// refresh. Reference is optional; when empty the daemon uses the
// default XDG baseline path for the upstream. The remaining fields
// match the snapshot/diff CLI filters.
type MITMDriftUpstreamCfg struct {
	Reference       string   `json:"reference" toml:"reference"`
	IncludeUA       []string `json:"includeUa,omitempty" toml:"include_ua,omitempty"`
	ExcludeUA       []string `json:"excludeUa,omitempty" toml:"exclude_ua,omitempty"`
	RequireBodyKeys []string `json:"requireBodyKeys,omitempty" toml:"require_body_keys,omitempty"`
	ForbidBodyKeys  []string `json:"forbidBodyKeys,omitempty" toml:"forbid_body_keys,omitempty"`
}

// EnabledFor reports whether the configured MITM provider set includes provider.
func (m MITMConfig) EnabledFor(provider string) bool {
	normalizedProvider := normalizeMITMProviderName(provider)
	if !isValidMITMProviderName(normalizedProvider) {
		return false
	}
	providers, err := parseMITMProviders(m.Providers)
	if err != nil {
		return false
	}
	if len(providers) == 1 && providers[0] == mitmProviderAll {
		return true
	}
	return slices.Contains(providers, normalizedProvider)
}

// Profile represents a named preset of session settings.
type Profile struct {
	Model          string       `json:"model,omitempty" toml:"model,omitempty"`
	PermissionMode string       `json:"permissionMode,omitempty" toml:"permission_mode,omitempty"`
	Permissions    *Permissions `json:"permissions,omitempty" toml:"permissions,omitempty"`
	OutputStyle    string       `json:"outputStyle,omitempty" toml:"output_style,omitempty"`
	// RemoteControl is a per profile override of the global default.
	// nil means inherit. false explicitly disables. true explicitly
	// enables.
	RemoteControl *bool `json:"remoteControl,omitempty" toml:"remote_control,omitempty"`
}

// Permissions represents the permissions configuration for sessions.
// Kept in config package to avoid circular imports with session package.
type Permissions struct {
	Allow                        []string `json:"allow,omitempty" toml:"allow,omitempty"`
	Ask                          []string `json:"ask,omitempty" toml:"ask,omitempty"`
	Deny                         []string `json:"deny,omitempty" toml:"deny,omitempty"`
	AdditionalDirectories        []string `json:"additionalDirectories,omitempty" toml:"additional_directories,omitempty"`
	DefaultMode                  string   `json:"defaultMode,omitempty" toml:"default_mode,omitempty"`
	DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode,omitempty" toml:"disable_bypass_permissions_mode,omitempty"`
}

// NewConfig creates a new Config with sensible defaults. The function uses
// a var declaration plus per-field assignment so each sub-block defaults to
// its zero value without forcing exhaustruct to walk the nested types. The
// loader fills the sub-blocks when the user supplies them.
func NewConfig() *Config {
	cfg := new(Config)
	cfg.Profiles = make(map[string]Profile)
	return cfg
}
