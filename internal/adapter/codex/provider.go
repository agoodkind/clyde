package codex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterretry "goodkind.io/clyde/internal/adapter/retry"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// Provider implements adapterprovider.Provider for the Codex
// websocket-only path. Construction binds the runtime dependencies
// once at daemon startup; Execute is the per-request entry point that
// stitches the websocket transport, the continuation ledger, and the
// normalized event emission together.
type Provider struct {
	cfg             config.AdapterCodex
	notices         config.AdapterNotices
	auth            adapterprovider.AuthLookup
	log             *slog.Logger
	httpClient      *http.Client
	now             func() time.Time
	sessionCache    *WebsocketSessionCache
	wsRegistry      *livetrack.Registry[WsSessionMeta]
	workspaceProbe  *WorkspaceProbe
	accountID       string
	bodyLog         BodyLogConfig
	bodyLogProvider BodyLogConfigProvider
	fileLog         FileLogRotationConfig
	retryPolicies   []adapterretry.Policy
}

// ProviderOptions extends the generic provider.Deps with Codex-only
// settings the dispatcher knows at construction time.
type ProviderOptions struct {
	AccountID        string
	BodyLog          BodyLogConfig
	BodyLogProvider  BodyLogConfigProvider
	FileLog          FileLogRotationConfig
	WsSessionIdleTTL time.Duration
	// WsSessionRegistry is the per-daemon livetrack registry for
	// tracking live Codex websocket connections so daemon reload can
	// drain or force-close them. When nil, CloseAll is the only
	// shutdown mechanism.
	WsSessionRegistry *livetrack.Registry[WsSessionMeta]
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
		cfg:             deps.Config.Codex,
		notices:         deps.Config.Notices,
		auth:            deps.Auth,
		log:             log,
		httpClient:      httpClient,
		now:             now,
		sessionCache:    NewWebsocketSessionCache(log, idleTTL, wsReg),
		wsRegistry:      wsReg,
		workspaceProbe:  NewWorkspaceProbe(),
		accountID:       strings.TrimSpace(opts.AccountID),
		bodyLog:         opts.BodyLog,
		bodyLogProvider: opts.BodyLogProvider,
		fileLog:         opts.FileLog,
		retryPolicies:   adapterretry.FromConfig(deps.Config.Retry),
	}
}

// CloseAllSessions closes every cached websocket connection. ctx is
// passed to livetrack release events. When a WsSessionRegistry was
// supplied at construction time, it delegates to ForceCloseMatching
// via the registry so force-close is bounded. When nil, it falls back
// to the legacy CloseAll path.
func (p *Provider) CloseAllSessions(ctx context.Context, reason string) {
	if p == nil || p.sessionCache == nil {
		return
	}
	p.sessionCache.CloseAll(ctx, reason)
}

// DrainSessions blocks until all cached websocket sessions complete or
// ctx expires. When the registry was not supplied, it falls back to the
// legacy CloseAllSessions. Callers (adapter.Server.Shutdown) should
// prefer this so the reload deadline participates in livetrack drain
// accounting.
func (p *Provider) DrainSessions(ctx context.Context, reason string) {
	if p == nil {
		return
	}
	if p.wsRegistry != nil {
		result := p.wsRegistry.Drain(ctx, reason)
		if p.log != nil {
			p.log.InfoContext(ctx, "adapter.codex.ws_sessions.drained",
				"component", "adapter",
				"subcomponent", "codex",
				"final", result.Final.String(),
				"remaining", result.Remaining,
				"force_closed", result.ForceClosed,
				"duration_ms", result.Duration.Milliseconds(),
			)
		}
		return
	}
	p.CloseAllSessions(ctx, reason)
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

// Execute satisfies adapterprovider.Provider.Execute. It builds a
// DirectConfig from the provider's deps, runs the websocket transport
// via RunDirect, and surfaces the result as adapterprovider.Result.
func (p *Provider) Execute(ctx context.Context, req adapterresolver.ResolvedRequest, w adapterprovider.EventWriter) (adapterprovider.Result, error) {
	if p == nil {
		return adapterprovider.Result{}, ErrCodexProviderNotConfigured
	}
	if p.auth == nil {
		return adapterprovider.Result{}, adapterprovider.ErrAuthMissing
	}

	token, err := p.auth.Token(ctx)
	if err != nil {
		p.log.WarnContext(ctx, "adapter.codex.auth_lookup_failed",
			"component", "adapter",
			"subcomponent", "codex_provider",
			"err", err.Error(),
		)
		return adapterprovider.Result{}, fmt.Errorf("codex provider: auth lookup: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return adapterprovider.Result{}, adapterprovider.ErrAuthMissing
	}

	directCfg := DirectConfig{
		HTTPClient:                     p.httpClient,
		BaseURL:                        codexBaseURL(p.cfg.BaseURL),
		WebsocketEnabled:               true,
		WebsocketURL:                   codexWebsocketURL(p.cfg.BaseURL),
		Token:                          token,
		AccountID:                      p.accountID,
		RequestID:                      codexRequestID(req),
		CursorRequestID:                req.Cursor.RequestID,
		Correlation:                    req.Correlation,
		WorkspacePath:                  req.Cursor.WorkspacePath,
		WorkspaceProbe:                 p.workspaceProbe,
		SessionCache:                   p.sessionCache,
		Log:                            p.log,
		BodyLog:                        p.bodyLog,
		BodyLogProvider:                p.bodyLogProvider,
		FileLog:                        p.fileLog,
		ReasoningSummary:               p.cfg.ReasoningSummary,
		InboundThinkingMaterialization: codexSummaryRenderStrategy(p.cfg.Reasoning.ResolvedRoundTripSummary()),
		WireCaptureMode:                WireCaptureMode(p.cfg.ResolvedCodexWireCaptureMode()),
		RoundTripEncrypted:             RoundTripEncrypted(p.cfg.Reasoning.ResolvedRoundTripEncrypted()),
		RoundTripSummary:               RoundTripSummary(p.cfg.Reasoning.ResolvedRoundTripSummary()),
		RetryPolicies:                  p.retryPolicies,
		// BeforeAttempt is injected by the adapter dispatch layer via
		// context so the server can register each retry attempt as a
		// nested livetrack egress session without changing this interface.
		BeforeAttempt: beforeAttemptFromContext(ctx),
	}

	warningWindows, usageWarningErr := ProbeUsageWarnings(ctx, usageWarningProbeConfig{
		HTTPClient: directCfg.HTTPClient,
		BaseURL:    directCfg.BaseURL,
		Token:      directCfg.Token,
		AccountID:  directCfg.AccountID,
		Now:        p.now,
	})
	if usageWarningErr != nil {
		p.log.WarnContext(ctx, "adapter.codex.usage_warning_probe_failed",
			"component", "adapter",
			"subcomponent", "codex_provider",
			"request_id", directCfg.RequestID,
			"err", usageWarningErr,
		)
	}

	model := resolvedModelFromRequest(req)
	runResult, runErr := RunDirect(ctx, directCfg, req.OpenAI, model, req.Effort.String(), w.WriteEvent)
	if runErr != nil {
		return adapterprovider.Result{}, runErr
	}
	if flushErr := w.Flush(); flushErr != nil {
		return adapterprovider.Result{}, flushErr
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
		UsageNoticeWindows:         warningWindows,
	}, nil
}

func codexRequestID(req adapterresolver.ResolvedRequest) string {
	if strings.TrimSpace(req.RequestID) != "" {
		return strings.TrimSpace(req.RequestID)
	}
	return strings.TrimSpace(req.Cursor.RequestID)
}

// resolvedModelFromRequest reconstructs the legacy
// adaptermodel.ResolvedModel surface from the typed ResolvedRequest.
// The websocket transport still consumes ResolvedModel today; Plan 7
// removes that dependency.
func resolvedModelFromRequest(req adapterresolver.ResolvedRequest) adaptermodel.ResolvedModel {
	return adaptermodel.ResolvedModel{
		Alias:           req.Model,
		Backend:         adaptermodel.BackendCodex,
		ClaudeModel:     req.Model,
		Context:         req.ContextBudget.InputTokens,
		Effort:          req.Effort.String(),
		MaxOutputTokens: req.ContextBudget.OutputTokens,
		FamilySlug:      req.Family,
		Instructions:    req.Instructions,
	}
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
