package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterretry "goodkind.io/clyde/internal/adapter/retry"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm/capture"
)

// Provider implements adapterprovider.Provider for the Codex Responses path.
// Construction binds the runtime dependencies
// once at daemon startup; Execute is the per-request entry point that
// stitches the websocket transport, the continuation ledger, and the
// normalized event emission together.
type Provider struct {
	cfg            config.AdapterCodex
	notices        config.AdapterNotices
	auth           adapterprovider.AuthLookup
	log            *slog.Logger
	httpClient     *http.Client
	now            func() time.Time
	sessionCache   *WebsocketSessionCache
	wsRegistry     *livetrack.Registry[WsSessionMeta]
	workspaceProbe *WorkspaceProbe
	accountID      string
	fileLog        FileLogRotationConfig
	retryPolicies  []adapterretry.Policy
	// wireBaselineLoader projects the codex-cli flavor from the capture
	// store's baseline at request time with updated-at caching, so a
	// baseline learned or refreshed after daemon startup is picked up
	// without a restart.
	wireBaselineLoader *WireBaselineLoader
	// captureStore receives one capture.Record per outbound Codex
	// exchange tagged client="adapter.codex" and serves the codex-cli wire
	// baseline reads. Nil disables both recording and baseline-driven
	// identity.
	captureStore *capture.Store
	// stripWireFlags lists capability tokens to drop from the outbound
	// codex capability headers, from the provider-neutral
	// [adapter].strip_wire_flags config. Empty replays the learned headers
	// untouched.
	stripWireFlags []string
}

// ProviderOptions extends the generic provider.Deps with Codex-only
// settings the dispatcher knows at construction time.
type ProviderOptions struct {
	AccountID        string
	FileLog          FileLogRotationConfig
	WsSessionIdleTTL time.Duration
	// WsSessionRegistry is the per-daemon livetrack registry for
	// tracking live Codex websocket connections so daemon reload can
	// drain or force-close them. Required.
	WsSessionRegistry *livetrack.Registry[WsSessionMeta]
	// CaptureStore, when non-nil, receives one [capture.Record] per
	// outbound Codex exchange tagged client="adapter.codex" and serves the
	// codex-cli wire baseline reads (current baseline + updated-at). The
	// daemon sets it from its shared capture store; a nil store records
	// nothing and disables baseline-driven identity. Unlike the Anthropic
	// path, a missing or invalid baseline is NOT fatal: the egress falls
	// back to compiled-in constants so a cold-start codex still works.
	CaptureStore *capture.Store
}

const defaultWsSessionIdleTTL = 10 * time.Minute

// NewProvider constructs the Codex Provider.
func NewProvider(deps adapterprovider.Deps, opts ProviderOptions) *Provider {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	httpClient := deps.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	idleTTL := opts.WsSessionIdleTTL
	if idleTTL <= 0 {
		idleTTL = defaultWsSessionIdleTTL
	}
	ConfigureCodexFileLogger(opts.FileLog)
	wsReg := opts.WsSessionRegistry
	return &Provider{
		cfg:                deps.Config.Codex,
		notices:            deps.Config.Notices,
		auth:               deps.Auth,
		log:                log,
		httpClient:         httpClient,
		now:                now,
		sessionCache:       NewWebsocketSessionCache(log, idleTTL, wsReg),
		wsRegistry:         wsReg,
		workspaceProbe:     NewWorkspaceProbe(),
		accountID:          strings.TrimSpace(opts.AccountID),
		fileLog:            opts.FileLog,
		retryPolicies:      appendBuiltinCodexRetryPolicies(adapterretry.FromConfig(deps.Config.Retry)),
		wireBaselineLoader: NewWireBaselineLoader(),
		captureStore:       opts.CaptureStore,
		stripWireFlags:     deps.Config.StripWireFlags,
	}
}

// ID satisfies adapterprovider.Provider.
func (p *Provider) ID() adapterresolver.ProviderID { return adapterresolver.ProviderCodex }

// ErrCodexProviderNotConfigured signals that the Provider was
// constructed without the dependencies it needs to make a wire call.
// Today that means missing AuthLookup or empty BaseURL/WebsocketURL.
var ErrCodexProviderNotConfigured = errors.New("codex provider: not configured")

// beforeAttemptContextKey is the context key used by the adapter
// dispatch layer to pass a BeforeAttempt hook into Execute without
// modifying the adapterprovider.Provider interface.
type beforeAttemptContextKey struct{}

// WithBeforeAttempt stores a BeforeAttempt hook in ctx so Execute can
// forward it to the websocket transport's retry loop. The hook is called
// once per retry attempt so the caller can register each attempt as a
// nested livetrack egress session.
func WithBeforeAttempt(ctx context.Context, hook func(ctx context.Context, attemptNo int) (context.Context, func(string))) context.Context {
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, beforeAttemptContextKey{}, hook)
}

// beforeAttemptFromContext extracts the BeforeAttempt hook from ctx.
// Returns nil when the context carries no hook.
func beforeAttemptFromContext(ctx context.Context) func(context.Context, int) (context.Context, func(string)) {
	v, _ := ctx.Value(beforeAttemptContextKey{}).(func(context.Context, int) (context.Context, func(string)))
	return v
}

// Execute satisfies adapterprovider.Provider.Execute by preparing then
// executing the exact provider-owned transport payload.
func (p *Provider) Execute(ctx context.Context, req adapterresolver.ResolvedRequest, w adapterprovider.EventWriter) (adapterprovider.Result, error) {
	prepared, err := p.Prepare(req)
	if err != nil {
		return adapterprovider.Result{}, err
	}
	return p.ExecutePrepared(ctx, prepared, w)
}

// ExecutePrepared performs Codex runtime work and executes the exact payload
// returned by Prepare without invoking the request builder again.
func (p *Provider) ExecutePrepared(ctx context.Context, prepared PreparedRequest, w adapterprovider.EventWriter) (adapterprovider.Result, error) {
	if p == nil {
		return adapterprovider.Result{}, ErrCodexProviderNotConfigured
	}
	if p.auth == nil {
		return adapterprovider.Result{}, adapterprovider.ErrAuthMissing
	}

	token, err := p.auth.Token(ctx)
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.auth_lookup_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex_provider",
			"err", err.Error(),
		)
		return adapterprovider.Result{}, fmt.Errorf("codex provider: auth lookup: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return adapterprovider.Result{}, adapterprovider.ErrAuthMissing
	}

	// AuthRefresh is wired only when the configured AuthLookup also
	// satisfies AuthRefresher. The transports tolerate a nil hook (they
	// fall back to retry-without-refresh), so this stays optional and
	// does not change the AuthLookup contract.
	var authRefresh func(context.Context) (string, error)
	if refresher, ok := p.auth.(adapterprovider.AuthRefresher); ok {
		authRefresh = refresher.ForceRefresh
	}

	directCfg := DirectConfig{
		NativePatchRepresentation:      "",
		HTTPClient:                     p.httpClient,
		BaseURL:                        codexBaseURL(p.cfg.BaseURL),
		WebsocketEnabled:               p.cfg.WebsocketEnabled,
		WebsocketURL:                   codexWebsocketURL(p.cfg.BaseURL),
		Token:                          token,
		AccountID:                      p.accountID,
		RequestID:                      codexRequestID(prepared.Resolved),
		CursorRequestID:                prepared.Resolved.Cursor.RequestID,
		Correlation:                    prepared.Resolved.Correlation,
		WorkspacePath:                  prepared.Resolved.Cursor.WorkspacePath,
		WorkspaceProbe:                 p.workspaceProbe,
		SessionCache:                   p.sessionCache,
		Log:                            p.log,
		FileLog:                        p.fileLog,
		ReasoningSummary:               p.cfg.ReasoningSummary,
		InboundThinkingMaterialization: codexSummaryRenderStrategy(p.cfg.Reasoning.ResolvedRoundTripSummary()),
		RoundTripEncrypted:             RoundTripEncrypted(p.cfg.Reasoning.ResolvedRoundTripEncrypted()),
		RoundTripSummary:               RoundTripSummary(p.cfg.Reasoning.ResolvedRoundTripSummary()),
		RetryPolicies:                  p.retryPolicies,
		// BeforeAttempt is injected by the adapter dispatch layer via
		// context so the server can register each retry attempt as a
		// nested livetrack egress session without changing this interface.
		BeforeAttempt:  beforeAttemptFromContext(ctx),
		AuthRefresh:    authRefresh,
		WireIdentity:   p.resolveWireIdentity(ctx),
		CaptureStore:   p.captureStore,
		StripWireFlags: p.stripWireFlags,
	}

	warningWindows, usageWarningErr := ProbeUsageWarnings(ctx, usageWarningProbeConfig{
		HTTPClient: directCfg.HTTPClient,
		BaseURL:    directCfg.BaseURL,
		Token:      directCfg.Token,
		AccountID:  directCfg.AccountID,
		Now:        p.now,
	})
	if usageWarningErr != nil {
		p.log.WarnContext(ctx, "adapter.codex.usage_warning_probe_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex_provider",
			"request_id", directCfg.RequestID,
			"err", usageWarningErr,
		)
	}

	runResult, runErr := runPrepared(ctx, directCfg, prepared.Transport, &prepared.Resolved, w.WriteEvent)
	if runErr != nil {
		return adapterprovider.Result{}, runErr
	}
	if flushErr := w.Flush(); flushErr != nil {
		return adapterprovider.Result{}, fmt.Errorf("flush codex provider events: %w", flushErr)
	}
	return adapterprovider.Result{
		Usage:                      runResult.Usage,
		FinishReason:               runResult.FinishReason,
		ReasoningSignaled:          runResult.ReasoningSignaled,
		ReasoningVisible:           runResult.ReasoningVisible,
		DerivedCacheCreationTokens: runResult.DerivedCacheCreationTokens,
		UpstreamResponseID:         runResult.ResponseID,
		ToolCallCount:              runResult.ToolCallCount,
		ToolCallNames:              runResult.ToolCallNames,
		HasSubagentToolCall:        runResult.HasSubagentToolCall,
		UsageNoticeWindows:         warningWindows, FinalResponse: nil, SystemFingerprint: "", ReasoningSummary: "", UsageNotices: nil,
	}, nil
}

// resolveWireIdentity projects the codex-cli outbound wire identity from
// the daemon-owned MITM baseline. When no baseline is wired, or the
// baseline is missing or invalid, it returns the zero-value identity so
// the egress falls back to the compiled-in identity constants and a
// cold-start codex still works. Only an unexpected error (a stat or
// parse failure on a configured path) is logged, and even then the
// fallback keeps the request working.
func (p *Provider) resolveWireIdentity(ctx context.Context) WireIdentity {
	var fallback WireIdentity
	if p == nil || p.captureStore == nil {
		return fallback
	}
	if p.wireBaselineLoader == nil {
		p.wireBaselineLoader = NewWireBaselineLoader()
	}
	identity, err := p.wireBaselineLoader.Load(ctx, p.captureStore)
	if err != nil {
		if errors.Is(err, ErrCodexBaselineInvalid) {
			p.log.WarnContext(ctx, "adapter.codex.wire_baseline.fallback_constants", "concern", codexWireBaselineConcern, "component", "adapter",
				"subcomponent", "codex_provider",
				"err", err.Error(),
			)
		}
		// Missing baseline is the expected cold-start case; the loader
		// already logged it at Debug. Either way fall back to constants.
		return fallback
	}
	return identity
}

func codexRequestID(req adapterresolver.ResolvedRequest) string {
	if strings.TrimSpace(req.RequestID) != "" {
		return strings.TrimSpace(req.RequestID)
	}
	return strings.TrimSpace(req.Cursor.RequestID)
}

// codexSummaryRenderStrategy translates the typed Codex round-trip summary
// enum into the render-package materialization strategy that drives the
// existing inbound materialization site. Codex's native value
// `native_summary_field` maps onto render's native thinking-block path
// because that is the existing materialization for native upstream
// reasoning fields. The encrypted_content lever is consumed in Phase 4
// separately and is not threaded through this site.
func codexSummaryRenderStrategy(strategy config.CodexRoundTripSummary) adapterrender.MaterializationStrategy {
	switch strategy {
	case config.CodexRoundTripSummaryDrop:
		return adapterrender.MaterializeDrop
	case config.CodexRoundTripSummaryPlainText:
		return adapterrender.MaterializePlainTextConcat
	case config.CodexRoundTripSummaryNative:
		return adapterrender.MaterializeNativeThinkingBlock
	default:
		return adapterrender.MaterializeNativeThinkingBlock
	}
}

// codexBaseURL applies the documented default for the Codex Responses
// HTTP endpoint when the config leaves BaseURL empty.
func codexBaseURL(raw string) string {
	if v := strings.TrimSpace(raw); v != "" {
		return v
	}
	return "https://chatgpt.com/backend-api/codex/responses"
}

// codexWebsocketURL converts a Codex base URL into the matching
// websocket URL. https:// becomes wss://; http:// becomes ws://; any
// other scheme passes through.
func codexWebsocketURL(raw string) string {
	base := codexBaseURL(raw)
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base
}

// WebsocketURL returns the resolved Codex websocket URL for use in
// egress session metadata labels. The adapter dispatch layer calls this
// when constructing EgressMeta; the per-provider config URL is only
// available inside the Provider, so this export returns the
// config-driven default as a best-effort URL label.
func WebsocketURL(raw string) string {
	return codexWebsocketURL(raw)
}
