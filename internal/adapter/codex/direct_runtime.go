package codex

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterretry "goodkind.io/clyde/internal/adapter/retry"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

// DirectConfig is part of Clyde's typed adapter surface.
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
	FileLog          FileLogRotationConfig
	ReasoningSummary string
	// InboundThinkingMaterialization picks how round-tripped synthetic
	// thinking envelopes on assistant content are shaped before forwarding
	// upstream. Empty string falls through to the Codex default (drop)
	// inside [BuildRequestWithConfig] / [SanitizeForUpstreamCacheWithStrategy].
	InboundThinkingMaterialization adapterrender.MaterializationStrategy
	// CaptureStore, when non-nil, receives one capture.Record per outbound
	// Codex exchange tagged client="adapter.codex". Nil records nothing.
	CaptureStore *capture.Store
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
	// NativePatchRepresentation is selected from the resolved Cursor model
	// route and carried unchanged to the Codex SSE parser.
	NativePatchRepresentation adapterrender.NativePatchRepresentation
	RetryPolicies             []adapterretry.Policy
	// BeforeAttempt, when non-nil, is forwarded to
	// WebsocketTransportConfig so the outer caller (adapter.Server)
	// can register each retry attempt as a nested livetrack session
	// without the codex package importing adapter internals.
	BeforeAttempt func(ctx context.Context, attemptNo int) (context.Context, func(string))
	// AuthRefresh, when non-nil, is called by the transports when the
	// upstream rejects the cached token with HTTP 401 or 403. It
	// returns a fresh access token (or an error if refresh failed) so
	// the transport can re-dial once before propagating the failure.
	AuthRefresh func(ctx context.Context) (string, error)
	// WireIdentity carries the baseline-driven outbound wire identity
	// the Provider projected from the daemon-owned MITM codex-cli
	// baseline. Empty fields fall back to the compiled-in identity
	// constants, so a zero-value WireIdentity preserves the cold-start
	// behavior.
	WireIdentity WireIdentity
	// StripWireFlags lists capability tokens to drop from the outbound
	// codex capability headers, from the provider-neutral
	// [adapter].strip_wire_flags config. Empty replays the learned headers
	// untouched.
	StripWireFlags []string
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

// runPrepared executes a transport payload built by the provider-owned
// preparation path. It may attach execution-local metadata to its value copy,
// but it never invokes the request builder.
func runPrepared(
	ctx context.Context,
	cfg DirectConfig,
	transportPayload HTTPTransportRequest,
	resolved *adapterresolver.ResolvedRequest,
	emit func(adapterrender.Event) error,
) (RunResult, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	ConfigureCodexFileLogger(cfg.FileLog)

	cfg.NativePatchRepresentation = nativePatchRepresentationForCursorRoute(resolved)
	// WARNING: this is the websocket session identity, not the
	// prompt_cache_key. Codex uses prompt_cache_key for upstream cache
	// partitioning, but websocket previous_response_id reuse is only safe
	// when keyed by a real Cursor/Codex conversation/thread id. Content or
	// account-derived cache keys can be shared by unrelated fresh chats.
	conversationID := strings.TrimSpace(transportPayload.WebsocketSessionKey)
	installationID, _ := LoadInstallationID()
	if conversationID != "" {
		turnMeta := NewTurnMetadata(conversationID, "")
		if ws := strings.TrimSpace(cfg.WorkspacePath); ws != "" {
			entry := TurnMetadataWorkspace{
				AssociatedRemoteURLs: TurnMetadataRemoteURLs{Origin: ""},

				LatestGitCommitHash: "", HasChanges: false,
			}
			if cfg.WorkspaceProbe != nil {
				entry = cfg.WorkspaceProbe.Probe(ctx, ws)
			}
			turnMeta = turnMeta.WithWorkspace(ws, entry)
		}
		turnMetaJSON, _ := turnMeta.MarshalCompact()
		transportPayload.ClientMetadata = ClientMetadataWithTurn(installationID, WindowID(conversationID), turnMetaJSON)
	}

	httpCfg := HTTPTransportConfig{
		URL:                       cfg.BaseURL,
		HTTPClient:                cfg.HTTPClient,
		Token:                     cfg.Token,
		RequestID:                 cfg.RequestID,
		CursorRequestID:           cfg.CursorRequestID,
		Correlation:               cfg.Correlation,
		Alias:                     codexResolvedModelName(resolved),
		ConversationID:            conversationID,
		InstallationID:            installationID,
		WindowID:                  WindowID(conversationID),
		TurnMetadata:              transportPayload.ClientMetadata.TurnMetadataValue(),
		Log:                       cfg.Log,
		RoundTripEncrypted:        cfg.RoundTripEncrypted,
		NativePatchRepresentation: cfg.NativePatchRepresentation,
		RetryPolicies:             cfg.RetryPolicies,
		BeforeAttempt:             cfg.BeforeAttempt,
		AuthRefresh:               cfg.AuthRefresh,
		WireIdentity:              cfg.WireIdentity,
		CaptureStore:              cfg.CaptureStore,
		StripWireFlags:            cfg.StripWireFlags,
	}

	// Websocket transport disabled by config: use the HTTP/SSE transport
	// directly. This used to be a hard error with no fallback.
	if !cfg.WebsocketEnabled {
		return runHTTPTransportEvents(ctx, httpCfg, transportPayload, emit)
	}

	wsReq := ResponseCreateRequestFromHTTP(transportPayload)
	wsCfg := WebsocketTransportConfig{
		URL:                       cfg.WebsocketURL,
		Token:                     cfg.Token,
		AccountID:                 cfg.AccountID,
		RequestID:                 cfg.RequestID,
		CursorRequestID:           cfg.CursorRequestID,
		Correlation:               cfg.Correlation,
		Alias:                     codexResolvedModelName(resolved),
		ConversationID:            conversationID,
		TurnState:                 NewTurnState(),
		TurnMetadata:              transportPayload.ClientMetadata.TurnMetadataValue(),
		Prewarm:                   false,
		PrewarmTimeout:            0,
		SessionCache:              cfg.SessionCache,
		Log:                       cfg.Log,
		RoundTripEncrypted:        cfg.RoundTripEncrypted,
		NativePatchRepresentation: cfg.NativePatchRepresentation,
		RetryPolicies:             cfg.RetryPolicies,
		BeforeAttempt:             cfg.BeforeAttempt,
		AuthRefresh:               cfg.AuthRefresh,
		WireIdentity:              cfg.WireIdentity,
		CaptureStore:              cfg.CaptureStore,
		StripWireFlags:            cfg.StripWireFlags,
	}
	result, err := RunWebsocketTransportEvents(ctx, wsCfg, wsReq, emit)
	if errors.Is(err, ErrWebsocketFallbackToHTTP) {
		// Upstream returned HTTP 426 on the websocket upgrade. codex-rs
		// falls back to the HTTP/SSE transport here, so Clyde does too.
		return runHTTPTransportEvents(ctx, httpCfg, transportPayload, emit)
	}
	return result, err
}

func nativePatchRepresentationForCursorRoute(resolved *adapterresolver.ResolvedRequest) adapterrender.NativePatchRepresentation {
	if resolved == nil {
		return adapterrender.NativePatchRepresentationRaw
	}
	route := strings.TrimSpace(resolved.Cursor.NormalizedModel)
	if strings.HasPrefix(route, "clyde-codex-") {
		return adapterrender.NativePatchRepresentationJSON
	}
	return adapterrender.NativePatchRepresentationRaw
}
