package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic/anthmode"
)

// Config represents the clyde configuration.
type Config struct {
	// Logging configures process-wide runtime behavior.
	Logging LoggingConfig `json:"logging" toml:"logging"`
	// Conversation configures raw conversation indexing and background sync.
	Conversation ConversationConfig `json:"conversation" toml:"conversation"`
	// Adapter configures the OpenAI compatible HTTP adapter mounted
	// inside the daemon process.
	Adapter AdapterConfig `json:"adapter" toml:"adapter"`
	// MITM configures the local capture proxy used for provider
	// subprocesses and for adapter-side request observability.
	MITM MITMConfig `json:"mitm" toml:"mitm"`
	// Debug configures opt-in daemon diagnostics such as the loopback
	// pprof endpoint. It is empty by default, so no debug surface is exposed.
	Debug DebugConfig `json:"debug" toml:"debug"`
}

// DebugConfig holds opt-in daemon diagnostics. Everything here is off by
// default; an operator enables a facet by setting its value.
type DebugConfig struct {
	// PProfAddr, when set to a loopback address such as "[::1]:6060", makes
	// the daemon serve net/http/pprof there. Empty means the pprof endpoint
	// is not started. The CLYDE_DEBUG_PPROF_ADDR environment variable
	// overrides this when set.
	PProfAddr string `json:"pprof_addr" toml:"pprof_addr"`
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
	// Port defaults to 11434 (shared with Ollama conventions). Requests
	// arriving on this port are tagged ingress=openai.
	Port int `json:"port,omitempty" toml:"port,omitempty"`
	// CursorIngressPort, when greater than zero, binds a second adapter
	// listener on Host:port. Requests arriving there are tagged
	// ingress=cursor while the primary Port is tagged ingress=openai, so
	// Cursor BYOK traffic is separated from generic OpenAI-compatible
	// clients and keeps the dual-surface streaming reasoning render
	// path on that listener. The primary listener serves generic
	// OpenAI-compatible clients with reasoning_content-only streaming
	// reasoning. Zero (default) binds only the primary listener.
	CursorIngressPort int `json:"cursorIngressPort,omitempty" toml:"cursor_ingress_port,omitempty"`
	// DefaultModel is the fallback when a request does not name one.
	DefaultModel string `json:"defaultModel,omitempty" toml:"default_model,omitempty"`
	// MaxConcurrent caps concurrently handled adapter requests.
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
	// at the configured messages URL using the current platform's
	// Claude login credential store.
	DirectOAuth bool `json:"directOauth,omitempty" toml:"direct_oauth,omitempty"`
	// CaptureIngress, when true, persists the raw OpenAI-shape inbound
	// /v1/chat/completions request and the rendered reply to the MITM
	// capture store tagged client="adapter.ingress", in addition to the
	// always-on BYOK egress capture. Off by default because it roughly
	// doubles body volume in capture.db against the capture-store
	// retention cap. Requires the capture store to be open; with it nil
	// this knob does nothing.
	CaptureIngress bool `json:"captureIngress,omitempty" toml:"capture_ingress,omitempty"`
	// StripWireFlags lists capability tokens to drop from the outbound
	// provider capability header on egress (anthropic-beta on the Claude
	// path, x-codex-beta-features and openai-beta on the Codex path), which
	// all carry a comma-joined token list. It is provider-neutral and
	// applies to every provider; default empty replays the learned wire
	// identity untouched. Listing a token a provider requires for its
	// handshake (for example a Codex websocket-upgrade token) will break
	// that provider, so name only tokens the backend tolerates dropping.
	StripWireFlags []string `json:"stripWireFlags,omitempty" toml:"strip_wire_flags,omitempty"`
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
	// Anthropic carries Anthropic-specific sub-blocks under the canonical
	// [adapter.anthropic] root. Provider-specific endpoint metadata lives
	// at [adapter.anthropic.oauth], alongside wire-capture and reasoning
	// levers.
	Anthropic AdapterAnthropic `json:"anthropic,omitzero" toml:"anthropic,omitempty"`
	// Retry declares operator-supplied adapter retry policies. This list
	// holds only what config provides; provider packages append their
	// own builtin policies at construction. There is no catch-all retry.
	Retry AdapterRetry `json:"retry,omitzero" toml:"retry,omitempty"`
	// Pricing maps a wire model id to its public per-token billing rates
	// expressed in dollars-per-million-tokens. The read-time cost
	// aggregator (internal/daemon provider stats) consumes this map to
	// translate recorded token counts into an estimated dollar cost.
	// Keys are matched against the recorded model id by longest-prefix,
	// so "claude-opus-4-8" covers "claude-opus-4-8[1m]" and snapshot
	// variants. Empty disables read-time cost estimation. There are no
	// compiled-in rate defaults; operators declare the table they trust.
	Pricing map[string]AdapterModelPricing `json:"pricing,omitempty" toml:"pricing,omitempty"`
}

// AdapterModelPricing declares one model's public list rates in
// dollars-per-million-tokens. The read-time cost aggregator converts
// these to microcents-per-token (dollars-per-MTok * 100) before summing
// recorded token counts. Cache write/read are separate rates because
// Anthropic bills cache creation above input and cache reads far below
// it; a provider that does not bill those (e.g. Codex cache reads) can
// leave them zero.
type AdapterModelPricing struct {
	// InputPerMTok is the list price per one million input tokens, in USD.
	InputPerMTok float64 `json:"inputPerMtok,omitempty" toml:"input_per_mtok,omitempty"`
	// OutputPerMTok is the list price per one million output tokens, in USD.
	OutputPerMTok float64 `json:"outputPerMtok,omitempty" toml:"output_per_mtok,omitempty"`
	// CacheWritePerMTok is the list price per one million cache-creation
	// tokens, in USD. Anthropic's 5m TTL list rate is the common value.
	CacheWritePerMTok float64 `json:"cacheWritePerMtok,omitempty" toml:"cache_write_per_mtok,omitempty"`
	// CacheReadPerMTok is the list price per one million cache-read
	// tokens, in USD.
	CacheReadPerMTok float64 `json:"cacheReadPerMtok,omitempty" toml:"cache_read_per_mtok,omitempty"`
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
	// NativeModelRouting controls how declared native OpenAI/Codex aliases
	// (the native_aliases / advertised_native_aliases entries under
	// [adapter.codex.models]) resolve. Empty and "off" reject them as
	// unknown models. "codex" routes through the direct Codex backend.
	// "passthrough_override" routes to NativeModelPassthroughOverride.
	// There is no model-name prefix guessing: an alias routes to Codex
	// only when it is declared.
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
	// Reasoning carries the per-provider reasoning round-trip levers for
	// the Codex backend. Codex Responses carries TWO independent levers
	// per codex-rs context_manager/history.rs:361-405: visible summary
	// text and encrypted_content memory blob.
	Reasoning AdapterCodexReasoning `json:"reasoning,omitzero" toml:"reasoning,omitempty"`
}

// AdapterAnthropic carries Anthropic-specific sub-blocks introduced after
// the original AdapterConfig shape stabilized.
type AdapterAnthropic struct {
	// OAuth holds Anthropic API URL, header metadata, and keychain label
	// for the direct-OAuth path. Claude login credentials come from the
	// current platform's normal Claude credential store.
	OAuth AdapterOAuth `json:"oauth,omitzero" toml:"oauth,omitempty"`
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

// AdapterCodexModel is part of Clyde's typed adapter surface.
type AdapterCodexModel struct {
	AliasPrefix string `json:"aliasPrefix,omitempty" toml:"alias_prefix,omitempty"`
	Model       string `json:"model,omitempty" toml:"model,omitempty"`
	// Backend names the routing backend for aliases generated from this
	// codex model. It is one of the model.BackendID wire values
	// (claude/anthropic/codex/passthrough_override); config cannot import
	// internal/adapter/model without an import cycle, so the value is held
	// as a string and converted to model.BackendID at registry build time.
	// Empty defaults to the codex backend.
	Backend          string                     `json:"backend,omitempty" toml:"backend,omitempty"`
	InstructionsFile string                     `json:"instructionsFile,omitempty" toml:"instructions_file,omitempty"`
	Instructions     string                     `json:"-" toml:"-"`
	Efforts          []string                   `json:"efforts,omitempty" toml:"efforts,omitempty"`
	MaxOutputTokens  int                        `json:"maxOutputTokens,omitempty" toml:"max_output_tokens,omitempty"`
	Contexts         []AdapterCodexModelContext `json:"contexts,omitempty" toml:"contexts,omitempty"`
}

// AdapterCodexModelContext is part of Clyde's typed adapter surface.
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

// AdapterNoticeRepeatMode is part of Clyde's typed adapter surface.
type AdapterNoticeRepeatMode string

const (
	// AdapterNoticeRepeatEvery is part of Clyde's typed adapter surface.
	AdapterNoticeRepeatEvery AdapterNoticeRepeatMode = "every"
	// AdapterNoticeRepeatOncePerThresholdUntilReset is part of Clyde's typed adapter surface.
	AdapterNoticeRepeatOncePerThresholdUntilReset AdapterNoticeRepeatMode = "once_per_threshold_until_reset"
	// AdapterNoticeRepeatTimeCooldown is part of Clyde's typed adapter surface.
	AdapterNoticeRepeatTimeCooldown AdapterNoticeRepeatMode = "time_cooldown"
	// AdapterNoticeRepeatTurnCooldown is part of Clyde's typed adapter surface.
	AdapterNoticeRepeatTurnCooldown AdapterNoticeRepeatMode = "turn_cooldown"
)

// AdapterNoticeRepeatPolicy is part of Clyde's typed adapter surface.
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

// UsageRepeatPolicyOrDefault is part of Clyde's typed adapter surface.
func (n AdapterNotices) UsageRepeatPolicyOrDefault() AdapterNoticeRepeatPolicy {
	policy := n.Usage.Repeat
	if policy.Mode == "" {
		policy.Mode = AdapterNoticeRepeatEvery
	}
	return policy
}

// AdapterOAuth holds Anthropic Messages endpoint metadata supplied by the
// operator. Claude login credentials come from the normal Claude credential
// store for the current platform.
type AdapterOAuth struct {
	MessagesURL      string `json:"messagesUrl,omitempty" toml:"messages_url,omitempty"`
	AnthropicBeta    string `json:"anthropicBeta,omitempty" toml:"anthropic_beta,omitempty"`
	AnthropicVersion string `json:"anthropicVersion,omitempty" toml:"anthropic_version,omitempty"`
	KeychainService  string `json:"keychainService,omitempty" toml:"keychain_service,omitempty"`
	// ToolResultCacheReferenceEnabled controls whether Clyde emits
	// tool_result.cache_reference on the direct Anthropic OAuth path.
	// Default is false because the live Anthropic /v1/messages OAuth
	// tool-followup path rejected this field in production and MITM
	// captures of the official Claude CLI succeeded without it.
	ToolResultCacheReferenceEnabled bool `json:"toolResultCacheReferenceEnabled,omitempty" toml:"tool_result_cache_reference_enabled,omitempty"`
}

// ValidateOAuthFields returns an error if any required field is empty.
func (o AdapterOAuth) ValidateOAuthFields() error {
	if o.MessagesURL == "" {
		return fmt.Errorf("adapter: [adapter.anthropic.oauth].messages_url must be set")
	}
	if o.AnthropicBeta == "" {
		return fmt.Errorf("adapter: [adapter.anthropic.oauth].anthropic_beta must be set")
	}
	if o.AnthropicVersion == "" {
		return fmt.Errorf("adapter: [adapter.anthropic.oauth].anthropic_version must be set")
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
	// ignored. Requires the selected learned flavor to carry Anthropic's
	// prompt-caching scope beta to be effective.
	PromptCacheScope string `json:"promptCacheScope,omitempty" toml:"prompt_cache_scope,omitempty"`
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
	// Backend names the routing backend for aliases generated from this
	// family. It is one of the model.BackendID wire values
	// (claude/anthropic/codex/passthrough_override); config cannot import
	// internal/adapter/model without an import cycle, so the value is held
	// as a string and converted to model.BackendID at registry build time.
	// Empty defaults to the claude backend (rewritten to anthropic when
	// direct_oauth is on). Set "codex" to route a family at the Codex
	// backend with a custom alias_prefix.
	Backend string `json:"backend,omitempty" toml:"backend,omitempty"`
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

// NewConfig creates a new Config with sensible defaults. The function uses
// a var declaration plus per-field assignment so each sub-block defaults to
// its zero value without forcing exhaustruct to walk the nested types. The
// loader fills the sub-blocks when the user supplies them.
func NewConfig() *Config {
	return new(Config)
}
