package codex

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterretry "goodkind.io/clyde/internal/adapter/retry"
	"goodkind.io/clyde/internal/correlation"
)

type DirectConfig struct {
	HTTPClient       *http.Client
	BaseURL          string
	WebsocketEnabled bool
	WebsocketURL     string
	Token            string
	AccountID        string
	RequestID        string
	CursorRequestID  string
	Correlation      correlation.Context
	// WorkspacePath is the absolute path to the Cursor-active
	// workspace, used to populate the `workspaces` block in
	// `x-codex-turn-metadata`. Empty when Cursor did not supply a
	// workspace.
	WorkspacePath string
	// WorkspaceProbe runs the small git probe (origin / HEAD /
	// has_changes). Optional. When nil, the workspace block is
	// emitted with the path only and no git fields.
	WorkspaceProbe *WorkspaceProbe
	// SessionCache enables persistent ws session reuse with chained
	// previous_response_id and delta input. Constructed once per
	// Provider. Required: RunDirect refuses to run without it.
	SessionCache     *WebsocketSessionCache
	Log              *slog.Logger
	BodyLog          BodyLogConfig
	BodyLogProvider  BodyLogConfigProvider
	FileLog          FileLogRotationConfig
	ReasoningSummary string
	// InboundThinkingMaterialization picks how round-tripped synthetic
	// thinking envelopes on assistant content are shaped before forwarding
	// upstream. Empty string falls through to the Codex default (drop)
	// inside [BuildRequestWithConfig] / [SanitizeForUpstreamCacheWithStrategy].
	InboundThinkingMaterialization adapterrender.MaterializationStrategy
	// WireCaptureMode controls the optional per-frame log of inbound
	// websocket bodies. Off (default) is safe; the other modes route to
	// adapter.providers.codex.wire_capture for short-retention diagnostics.
	WireCaptureMode WireCaptureMode
	// RoundTripEncrypted controls whether the renderer embeds the
	// encrypted_content blob on the synthetic-thinking close marker.
	// RoundTripEncryptedRoundTrip (the codex-rs default) emits the
	// `data-encrypted` attribute; RoundTripEncryptedDrop omits it. Empty
	// resolves to RoundTripEncryptedRoundTrip per codex-rs.
	RoundTripEncrypted RoundTripEncrypted
	// RoundTripSummary picks the inbound shape for round-tripped
	// synthetic thinking envelopes on the next outbound request. Empty
	// resolves to RoundTripSummaryNative per codex-rs.
	RoundTripSummary RoundTripSummary
	RetryPolicies    []adapterretry.Policy
	// BeforeAttempt, when non-nil, is forwarded to
	// WebsocketTransportConfig so the outer caller (adapter.Server)
	// can register each retry attempt as a nested livetrack session
	// without the codex package importing adapter internals.
	BeforeAttempt func(ctx context.Context, attemptNo int) (context.Context, func(string))
}

// RoundTripEncrypted is the closed enum the codex transport honors when the
// dispatcher passes the encrypted_content round-trip lever. Mirrors
// config.CodexRoundTripEncrypted value-for-value so the dispatcher does a
// typed string conversion at the boundary without an import edge.
type RoundTripEncrypted string

// Codex round-trip encrypted_content modes. RoundTripEncryptedRoundTrip is
// the documented codex-rs default; empty resolves to it.
const (
	RoundTripEncryptedRoundTrip RoundTripEncrypted = "round_trip"
	RoundTripEncryptedDrop      RoundTripEncrypted = "drop"
)

// RoundTripSummary is the closed enum the codex transport honors when the
// dispatcher passes the summary round-trip lever for synthetic thinking
// envelopes. Mirrors config.CodexRoundTripSummary value-for-value so the
// dispatcher does a typed string conversion at the boundary without an
// import edge.
type RoundTripSummary string

// Codex round-trip summary modes. RoundTripSummaryNative is the
// documented codex-rs default; empty resolves to it.
const (
	RoundTripSummaryNative    RoundTripSummary = "native_summary_field"
	RoundTripSummaryDrop      RoundTripSummary = "drop"
	RoundTripSummaryPlainText RoundTripSummary = "plain_text_concat"
)

// WireCaptureMode is the closed enum the codex transport honors when the
// dispatcher passes a per-provider wire-capture lever. Mirrors
// config.CodexWireCaptureMode value-for-value so the dispatcher does a
// typed string conversion at the boundary without an import edge.
type WireCaptureMode string

// Codex wire-capture modes. WireCaptureOff is the safe default and matches
// an empty configured value.
const (
	WireCaptureOff           WireCaptureMode = "off"
	WireCaptureSummaryOnly   WireCaptureMode = "summary_only"
	WireCaptureReasoningOnly WireCaptureMode = "reasoning_only"
	WireCaptureFull          WireCaptureMode = "full"
)

func RunDirect(
	ctx context.Context,
	cfg DirectConfig,
	req adapteropenai.ChatRequest,
	model adaptermodel.ResolvedModel,
	effort string,
	emit func(adapterrender.Event) error,
) (RunResult, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	ConfigureCodexFileLogger(cfg.FileLog)
	if !cfg.WebsocketEnabled {
		return NewRunResult("stop"), errCodexWebsocketDisabled
	}
	transportPayload := BuildRequestWithConfig(req, model, effort, RequestBuilderConfig{
		ReasoningSummary:               cfg.ReasoningSummary,
		InboundThinkingMaterialization: cfg.InboundThinkingMaterialization,
		RoundTripSummary:               cfg.RoundTripSummary,
		RoundTripEncrypted:             cfg.RoundTripEncrypted,
	})
	// WARNING: this is the websocket session identity, not the
	// prompt_cache_key. Codex uses prompt_cache_key for upstream cache
	// partitioning, but websocket previous_response_id reuse is only safe
	// when keyed by a real Cursor/Codex conversation/thread id. Content or
	// account-derived cache keys can be shared by unrelated fresh chats.
	conversationID := strings.TrimSpace(transportPayload.WebsocketSessionKey)
	if conversationID != "" {
		installationID, _ := LoadInstallationID()
		turnMeta := NewTurnMetadata(conversationID, "")
		if ws := strings.TrimSpace(cfg.WorkspacePath); ws != "" {
			entry := TurnMetadataWorkspace{}
			if cfg.WorkspaceProbe != nil {
				entry = cfg.WorkspaceProbe.Probe(ws)
			}
			turnMeta = turnMeta.WithWorkspace(ws, entry)
		}
		turnMetaJSON, _ := turnMeta.MarshalCompact()
		transportPayload.ClientMetadata = ClientMetadataWithTurn(installationID, CodexWindowID(conversationID), turnMetaJSON)
	}
	wsReq := ResponseCreateRequestFromHTTP(transportPayload)
	wsCfg := WebsocketTransportConfig{
		URL:                cfg.WebsocketURL,
		Token:              cfg.Token,
		AccountID:          cfg.AccountID,
		RequestID:          cfg.RequestID,
		CursorRequestID:    cfg.CursorRequestID,
		Correlation:        cfg.Correlation,
		Alias:              model.Alias,
		ConversationID:     conversationID,
		TurnState:          NewTurnState(),
		TurnMetadata:       transportPayload.ClientMetadata[CodexTurnMetadataHeader],
		BodyLog:            cfg.BodyLog,
		BodyLogProvider:    cfg.BodyLogProvider,
		SessionCache:       cfg.SessionCache,
		Log:                cfg.Log,
		WireCaptureMode:    cfg.WireCaptureMode,
		RoundTripEncrypted: cfg.RoundTripEncrypted,
		RetryPolicies:      cfg.RetryPolicies,
		BeforeAttempt:      cfg.BeforeAttempt,
	}
	return RunWebsocketTransportEvents(ctx, wsCfg, wsReq, emit)
}

var errCodexWebsocketDisabled = errors.New("codex websocket transport is disabled but no HTTPS fallback exists")
