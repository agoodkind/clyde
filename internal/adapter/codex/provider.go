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
	workspaceProbe  *WorkspaceProbe
	accountID       string
	bodyLog         BodyLogConfig
	bodyLogProvider BodyLogConfigProvider
	fileLog         FileLogRotationConfig
	retryPolicies   []adapterretry.Policy
}

// ProviderOptions extends the generic provider.Deps with Codex-only
// settings the dispatcher knows at construction time. Today: the
// account id (lifted from auth.json by daemon startup) and the
// websocket-session idle timeout.
type ProviderOptions struct {
	AccountID        string
	BodyLog          BodyLogConfig
	BodyLogProvider  BodyLogConfigProvider
	FileLog          FileLogRotationConfig
	WsSessionIdleTTL time.Duration
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
	return &Provider{
		cfg:             deps.Config.Codex,
		notices:         deps.Config.Notices,
		auth:            deps.Auth,
		log:             log,
		httpClient:      httpClient,
		now:             now,
		sessionCache:    NewWebsocketSessionCache(log, idleTTL),
		workspaceProbe:  NewWorkspaceProbe(),
		accountID:       strings.TrimSpace(opts.AccountID),
		bodyLog:         opts.BodyLog,
		bodyLogProvider: opts.BodyLogProvider,
		fileLog:         opts.FileLog,
		retryPolicies:   adapterretry.FromConfig(deps.Config.Retry),
	}
}

// CloseAllSessions is invoked on daemon shutdown so any cached
// websocket connections are closed before the daemon exits.
func (p *Provider) CloseAllSessions(reason string) {
	if p == nil || p.sessionCache == nil {
		return
	}
	p.sessionCache.CloseAll(reason)
}

// ID satisfies adapterprovider.Provider.
func (p *Provider) ID() adapterresolver.ProviderID { return adapterresolver.ProviderCodex }

// ErrCodexProviderNotConfigured signals that the Provider was
// constructed without the dependencies it needs to make a wire call.
// Today that means missing AuthLookup or empty BaseURL/WebsocketURL.
var ErrCodexProviderNotConfigured = errors.New("codex provider: not configured")

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
