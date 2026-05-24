package adapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/adapter/errcontract"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/oauthrotation"
	"goodkind.io/clyde/internal/slogger"
)

// boolPointer returns a heap-allocated copy of value so callers can
// hand off a *bool without exposing addresses of stack locals.
func boolPointer(value bool) *bool {
	copied := value
	return &copied
}

// DefaultPort is the loopback port the adapter listens on when
// AdapterConfig.Port is zero. The value matches the Ollama default
// so OPENAI_BASE_URL=http://localhost:11434/v1 flows work unchanged.
const DefaultPort = 11434

// DefaultHost is the loopback bind. The adapter never binds a public
// interface unless the user explicitly sets AdapterConfig.Host.
const DefaultHost = "::1"

// DefaultMaxConcurrent caps the number of in-flight adapter requests when the
// config omits a value.
const DefaultMaxConcurrent = 4

func (s *Server) evaluateUsageNotices(windows []adapterruntime.UsageWindowNoticeInput) []adapterruntime.UsageNotice {
	if s == nil || s.usageNoticeGate == nil {
		return nil
	}
	return s.usageNoticeGate.Evaluate(windows, s.cfg.Notices, adapterClock.Now())
}

// systemFingerprint is the value the adapter reports in the OpenAI
// response field of the same name. It changes when the binary is
// rebuilt so clients can detect a behavioral change. Kept stable
// across requests within one daemon run.
var systemFingerprint = "fp_clyde_" + adapterClock.Now().UTC().Format("20060102")

// Server is the HTTP facade. The daemon process creates one and
// either calls Start in a goroutine (production) or hands the
// handler to httptest.Server (tests).
type Server struct {
	cfg            config.AdapterConfig
	logprobs       config.AdapterLogprobs
	deps           Deps
	log            *slog.Logger
	requestLog     *logevent.Emitter
	logging        config.LoggingConfig
	runtimeLogging *RuntimeLogging
	registry       *Registry
	// egressRegistry tracks every in-flight outbound provider call
	// (Anthropic Messages HTTP, Codex Responses websocket, retry
	// attempts) so Shutdown can drain or force-close them under the
	// reload deadline.
	egressRegistry       *livetrack.Registry[EgressMeta]
	sem                  chan struct{}
	token                string
	mux                  *http.ServeMux
	httpSrv              *http.Server
	requests             *livetrack.Registry[IngressMeta]
	oauthRotator         *oauthrotation.Rotator
	anthr                *anthropic.Client
	httpClient           *http.Client
	ctxUsage             *contextUsageTracker
	usageNoticeGate      *adapterruntime.UsageNoticeGate
	providerRegistry     *adapterprovider.Registry
	codexProvider        *adaptercodex.Provider
	anthropicProvider    *anthropic.Provider
	errorRenderers       map[adapterRouteFamily]errcontract.ErrorRenderer
	streamErrorRenderers map[adapterRouteFamily]errcontract.StreamErrorRenderer
}

// New constructs a Server from the given adapter config. The deps
// hooks come from the daemon process so the adapter reuses existing
// binary resolution and scratch dir wiring. Returns an error when
// the registry cannot be built (missing families, default model, or
// required client_identity fields); the daemon refuses to start the
// listener in that case.
func New(ctx context.Context, cfg config.AdapterConfig, logging config.LoggingConfig, deps Deps, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernAdapterHTTPIngress)
	runtimeLogging := deps.RuntimeLogging
	if runtimeLogging == nil {
		runtimeLogging = NewRuntimeLogging(logging)
	}
	max := cfg.MaxConcurrent
	if max <= 0 {
		max = DefaultMaxConcurrent
	}
	token := cfg.RequireToken
	if v := os.Getenv("CLYDE_ADAPTER_TOKEN"); v != "" {
		token = v
	}
	registry, err := NewRegistry(cfg)
	if err != nil {
		return nil, err
	}
	adapterLog := log.With("subcomponent", "adapter")
	egressReg := newEgressRegistry()
	wsReg := adaptercodex.NewWsSessionRegistry()
	s := &Server{
		cfg:      cfg,
		logprobs: cfg.Logprobs,
		deps:     deps,
		log:      adapterLog,
		requestLog: logevent.NewEmitter(
			slogger.WithConcern(adapterLog, slogger.ConcernAdapterChatDispatch),
			logevent.RequiredLegsFromStrings(logging.Request.RequiredLegs),
			adapterEmitterOptions(logging.Request)...,
		),
		logging:        logging,
		runtimeLogging: runtimeLogging,
		registry:       registry,
		egressRegistry: egressReg,
		sem:            make(chan struct{}, max),
		token:          token,
		mux:            nil,
		httpSrv:        nil,
		requests:       newAdapterIngressRegistry(adapterLog),
		oauthRotator:   nil,
		anthr:          nil,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		ctxUsage:             newContextUsageTracker(),
		usageNoticeGate:      adapterruntime.NewUsageNoticeGateWithLogger(log),
		providerRegistry:     nil,
		codexProvider:        nil,
		anthropicProvider:    nil,
		errorRenderers:       defaultBoundaryRegistry.snapshotRenderers(),
		streamErrorRenderers: defaultBoundaryRegistry.snapshotStreamErrorRenderers(),
	}
	s.providerRegistry = adapterprovider.NewRegistry()
	probeCfg := config.NewConfigWithDefaults()
	probeCfg.Logging = logging
	probeCfg.Adapter.WireCapture = cfg.WireCapture
	policies, err := logpolicy.Resolve(*probeCfg)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelError, "adapter.logpolicy.resolve_failed", slog.String("err", err.Error()))
		return nil, fmt.Errorf("adapter: resolve logging policy: %w", err)
	}
	s.registerProviders(ctx, cfg, deps, log, max, wsReg, policies)
	s.mux = s.routes()
	return s, nil
}

// registerProviders wires the optional Codex and Anthropic providers
// into the server's provider registry. Extracted from [New] so that
// function stays under the funlen limit.
func (s *Server) registerProviders(
	ctx context.Context,
	cfg config.AdapterConfig,
	deps Deps,
	log *slog.Logger,
	maxConcurrent int,
	wsReg *livetrack.Registry[adaptercodex.WsSessionMeta],
	policies logpolicy.PolicySet,
) {
	if cfg.Codex.Enabled {
		codexAuth := adaptercodex.NewAuthManager(cfg.Codex.AuthFile, adaptercodex.AuthManagerOptions{
			HTTPClient: s.httpClient,
			Log:        slogger.WithConcern(log.With("subcomponent", "codex_auth"), slogger.ConcernAdapterProviderCodex),
			Now:        nil,
			RefreshURL: "",
		})
		s.codexProvider = adaptercodex.NewProvider(adapterprovider.Deps{
			Config:     cfg,
			Auth:       codexAuth,
			Logger:     slogger.WithConcern(log.With("subcomponent", "codex_provider"), slogger.ConcernAdapterProviderCodex),
			HTTPClient: s.httpClient,
			Telemetry:  nil,
			Now:        nil,
		}, codexProviderOptionsWithRegistry(wsReg, policies))
		s.providerRegistry.Register(s.codexProvider)
		log.LogAttrs(ctx, slog.LevelInfo, "adapter.provider_registry.registered",
			slog.String("provider", string(adapterresolver.ProviderCodex)),
			slog.Int("registered_count", len(s.providerRegistry.IDs())),
		)
	}
	if cfg.DirectOAuth {
		s.registerAnthropicProvider(ctx, cfg, deps, log, maxConcurrent, policies)
	}
}

// registerAnthropicProvider wires the Anthropic OAuth provider into
// the server's provider registry. Extracted from registerProviders to
// keep each helper under the funlen limit.
func (s *Server) registerAnthropicProvider(
	ctx context.Context,
	cfg config.AdapterConfig,
	deps Deps,
	log *slog.Logger,
	maxConcurrent int,
	policies logpolicy.PolicySet,
) {
	if deps.OAuthRotator != nil {
		// Single daemon-owned rotator: share the instance the daemon's
		// harvest-and-refresh loop also drives so on-disk renewals are
		// visible to the serve path through shared in-memory slots.
		s.oauthRotator = deps.OAuthRotator
	} else {
		s.oauthRotator = buildAnthropicRotator(cfg.OAuth, slogger.WithConcern(log.With("subcomponent", "oauth_rotation"), slogger.ConcernAdapterProviderAnthOAuth))
	}
	id := cfg.ClientIdentity
	messagesURL := cfg.OAuth.MessagesURL
	if override := strings.TrimSpace(deps.AnthropicMessagesURLOverride); override != "" {
		// Rewrite messages outbound through the MITM proxy. The
		// proxy's classifyRoute strips api.anthropic.com from
		// upstreamURL and prepends its own base, so we only need
		// to match the path the proxy expects (/v1/messages).
		messagesURL = strings.TrimRight(override, "/") + "/v1/messages"
		s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.oauth.mitm_routed",
			slog.String("messages_url", messagesURL),
		)
	}
	s.anthr = anthropic.New(nil, newRotatorTokenSource(s.oauthRotator, s.log), anthropic.Config{
		MessagesURL:             messagesURL,
		OAuthAnthropicVersion:   cfg.OAuth.AnthropicVersion,
		BetaHeader:              id.BetaHeader,
		UserAgent:               id.UserAgent,
		SystemPromptPrefix:      id.SystemPromptPrefix,
		StainlessPackageVersion: id.StainlessPackageVersion,
		StainlessRuntime:        id.StainlessRuntime,
		StainlessRuntimeVersion: id.StainlessRuntimeVersion,
		CCVersion:               id.CCVersion,
		CCEntrypoint:            id.CCEntrypoint,
		WireCaptureMode:         cfg.Anthropic.ResolvedAnthropicWireCaptureMode(),
		// The rotator implements ratelimitsink.Sink. The signal-emission
		// calls on the client side land in a later wave; wiring the field
		// now keeps the constructor stable and the rotator reachable.
		RateLimitSink: s.oauthRotator,
	})
	anthropicSidecarRotation := policies.Sinks[logpolicy.SinkAnthropicSidecar].Rotation
	s.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{
		Config: cfg,
		Logger: slogger.WithConcern(log.With("subcomponent", "anthropic_provider"), slogger.ConcernAdapterProviderAnthReq),
	}, anthropic.ProviderOptions{
		Prepare:         s.prepareAnthropicProviderRequest,
		ExecutePrepared: s.executeAnthropicPreparedRequest,
		FileLog: anthropic.FileLogRotationConfig{
			MaxSizeMB:  anthropicSidecarRotation.MaxSizeMB,
			MaxBackups: anthropicSidecarRotation.MaxBackups,
			MaxAgeDays: anthropicSidecarRotation.MaxAgeDays,
			Compress:   boolPointer(anthropicSidecarRotation.Compress),
		},
	})
	s.providerRegistry.Register(s.anthropicProvider)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.provider_registry.registered",
		slog.String("provider", string(adapterresolver.ProviderAnthropic)),
		slog.Int("registered_count", len(s.providerRegistry.IDs())),
	)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.oauth.enabled",
		slog.Int("max_concurrent", maxConcurrent),
	)
}

// codexProviderOptionsWithRegistry builds the codex provider's startup
// options from the daemon's logging config snapshots and the per-daemon
// ws-session registry. Extracted so [New] stays under the funlen budget.
func codexProviderOptionsWithRegistry(
	wsReg *livetrack.Registry[adaptercodex.WsSessionMeta],
	policies logpolicy.PolicySet,
) adaptercodex.ProviderOptions {
	codexSidecarRotation := policies.Sinks[logpolicy.SinkCodexSidecar].Rotation
	return adaptercodex.ProviderOptions{
		AccountID: "",
		FileLog: adaptercodex.FileLogRotationConfig{
			MaxSizeMB:  codexSidecarRotation.MaxSizeMB,
			MaxBackups: codexSidecarRotation.MaxBackups,
			MaxAgeDays: codexSidecarRotation.MaxAgeDays,
			Compress:   boolPointer(codexSidecarRotation.Compress),
		},
		WsSessionIdleTTL:  0,
		WsSessionRegistry: wsReg,
	}
}

func (s *Server) codexWebsocketEnabled() bool {
	return s.cfg.Codex.WebsocketEnabled
}

// adapterEmitterOptions converts the typed [config.LoggingRequest] into the
// [logevent.EmitterOption] set the adapter's request emitter uses.
//
// The production server passes a nil handler to [logevent.WithTestingTB]; only
// tests wire a non-nil handler to fail the active test under
// [logevent.IncompletePolicyFailTest]. The nil handler is intentional and keeps
// the test-fail integration option reachable from production callers.
func adapterEmitterOptions(request config.LoggingRequest) []logevent.EmitterOption {
	policy, ok := logevent.ParseIncompletePolicy(request.IncompletePolicy)
	if !ok {
		policy = logevent.IncompletePolicyWarn
	}
	return []logevent.EmitterOption{
		logevent.WithIncompletePolicy(policy),
		logevent.WithTestingTB(nil),
	}
}

// Addr returns the host:port the adapter will bind when Start is
// called.
func (s *Server) Addr() string {
	host := s.cfg.Host
	if host == "" {
		host = DefaultHost
	}
	port := s.cfg.Port
	if port <= 0 {
		port = DefaultPort
	}
	return net.JoinHostPort(normalizeListenHost(host), strconv.Itoa(port))
}

func normalizeListenHost(host string) string {
	trimmed := strings.TrimSpace(host)
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.Contains(inner, ":") {
			return inner
		}
	}
	return trimmed
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("timed out waiting for concurrency slot")
	}
}

func (s *Server) release() {
	select {
	case <-s.sem:
	default:
	}
}

var idLock sync.Mutex

func newRequestID() string {
	idLock.Lock()
	defer idLock.Unlock()
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// writeJSON serializes v to the response writer. The type parameter is
// constrained to any only because encoding/json takes any; call sites must
// pass a typed concrete value (no untyped composite literals such as
// map[string]any{...}) so the wire shape stays auditable.
func writeJSON[T any](w http.ResponseWriter, code int, v T) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
