package adapter

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/adapter/errcontract"
	"goodkind.io/clyde/internal/adapter/oauth"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/slogger"
)

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

type rawChatLogEvent struct {
	RequestID     string
	Method        string
	Path          string
	RemoteAddr    string
	Headers       map[string]string
	BodyBytes     int
	BodySummary   *BodySummary
	BodyRaw       string
	BodyB64       string
	BodyTruncated bool
	Correlation   correlation.Context
}

func (attrs rawChatLogEvent) asAttrs() []slog.Attr {
	out := []slog.Attr{
		slog.String("request_id", attrs.RequestID),
		slog.String("method", attrs.Method),
		slog.String("path", attrs.Path),
		slog.String("remote_addr", attrs.RemoteAddr),
		slog.Any("headers", attrs.Headers),
		slog.Int("body_bytes", attrs.BodyBytes),
	}
	if attrs.BodySummary != nil {
		out = append(out, slog.Any("body_summary", attrs.BodySummary))
	}
	if attrs.BodyRaw != "" {
		out = append(out, slog.String("body", attrs.BodyRaw))
	}
	if attrs.BodyB64 != "" {
		out = append(out, slog.String("body_b64", attrs.BodyB64))
	}
	if attrs.BodyTruncated {
		out = append(out, slog.Bool("body_truncated", true))
	}
	return correlation.AppendAttrs(out, attrs.Correlation)
}

func encodeBodyB64(body []byte, maxBytes int) string {
	if len(body) == 0 {
		return ""
	}
	if maxBytes <= 0 {
		return ""
	}
	if len(body) > maxBytes {
		body = body[:maxBytes]
	}
	return base64.StdEncoding.EncodeToString(body)
}

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
	cfg                  config.AdapterConfig
	logprobs             config.AdapterLogprobs
	deps                 Deps
	log                  *slog.Logger
	logging              config.LoggingConfig
	runtimeLogging       *RuntimeLogging
	registry             *Registry
	sem                  chan struct{}
	token                string
	mux                  *http.ServeMux
	httpSrv              *http.Server
	connMu               sync.Mutex
	conns                map[net.Conn]http.ConnState
	oauthMgr             *oauth.Manager
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
	if logging.Body.Mode == "" {
		logging.Body.Mode = "summary"
	}
	if logging.Body.MaxKB <= 0 {
		logging.Body.MaxKB = 32
	}
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
	s := &Server{
		cfg:            cfg,
		logprobs:       cfg.Logprobs,
		deps:           deps,
		log:            log.With("subcomponent", "adapter"),
		logging:        logging,
		runtimeLogging: runtimeLogging,
		registry:       registry,
		sem:            make(chan struct{}, max),
		token:          token,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		ctxUsage:             newContextUsageTracker(),
		usageNoticeGate:      adapterruntime.NewUsageNoticeGateWithLogger(log),
		errorRenderers:       defaultBoundaryRegistry.snapshotRenderers(),
		streamErrorRenderers: defaultBoundaryRegistry.snapshotStreamErrorRenderers(),
	}
	s.providerRegistry = adapterprovider.NewRegistry()
	if cfg.Codex.Enabled {
		s.codexProvider = adaptercodex.NewProvider(adapterprovider.Deps{
			Config:     cfg,
			Auth:       codexAuthLookup{server: s},
			Logger:     slogger.WithConcern(log.With("subcomponent", "codex_provider"), slogger.ConcernAdapterProviderCodex),
			HTTPClient: s.httpClient,
		}, codexProviderOptions(logging, runtimeLogging))
		s.providerRegistry.Register(s.codexProvider)
		log.LogAttrs(ctx, slog.LevelInfo, "adapter.provider_registry.registered",
			slog.String("provider", string(adapterresolver.ProviderCodex)),
			slog.Int("registered_count", len(s.providerRegistry.IDs())),
		)
	}
	if cfg.DirectOAuth {
		s.oauthMgr = oauth.NewManager(cfg.OAuth, "")
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
		s.anthr = anthropic.New(nil, s.oauthMgr, anthropic.Config{
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
		})
		s.anthropicProvider = anthropic.NewProvider(adapterprovider.Deps{
			Config: cfg,
			Logger: slogger.WithConcern(log.With("subcomponent", "anthropic_provider"), slogger.ConcernAdapterProviderAnthReq),
		}, anthropic.ProviderOptions{
			Prepare:         s.prepareAnthropicProviderRequest,
			ExecutePrepared: s.executeAnthropicPreparedRequest,
		})
		s.providerRegistry.Register(s.anthropicProvider)
		s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.provider_registry.registered",
			slog.String("provider", string(adapterresolver.ProviderAnthropic)),
			slog.Int("registered_count", len(s.providerRegistry.IDs())),
		)
		s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.oauth.enabled",
			slog.Int("max_concurrent", max),
		)
	}
	s.mux = s.routes()
	return s, nil
}

// codexProviderOptions builds the codex provider's startup options from
// the daemon's logging config snapshots. Extracted so [New] stays under
// the funlen budget.
func codexProviderOptions(logging config.LoggingConfig, runtimeLogging *RuntimeLogging) adaptercodex.ProviderOptions {
	return adaptercodex.ProviderOptions{
		AccountID: "",
		BodyLog:   adaptercodex.BodyLogConfig{Mode: logging.Body.Mode, MaxKB: logging.Body.MaxKB},
		BodyLogProvider: func() adaptercodex.BodyLogConfig {
			body := runtimeLogging.Body()
			return adaptercodex.BodyLogConfig{Mode: body.Mode, MaxKB: body.MaxKB}
		},
		FileLog: adaptercodex.FileLogRotationConfig{
			MaxSizeMB:  logging.Rotation.MaxSizeMB,
			MaxBackups: logging.Rotation.MaxBackups,
			MaxAgeDays: logging.Rotation.MaxAgeDays,
			Compress:   logging.Rotation.Compress,
		},
		WsSessionIdleTTL: 0,
	}
}

func resolveCodexAuthFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "~/.codex/auth.json"
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func (s *Server) codexWebsocketEnabled() bool {
	return s.cfg.Codex.WebsocketEnabled
}

type codexAuthFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccountID   string `json:"account_id"`
		AccessToken string `json:"access_token"`
	} `json:"tokens"`
}

func (s *Server) readCodexAuthFile() (codexAuthFile, error) {
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	p := resolveCodexAuthFile(s.cfg.Codex.AuthFile)
	data, err := os.ReadFile(p)
	if err != nil {
		log.Warn("adapter.codex.auth_file.read_failed",
			"subcomponent", "adapter",
			"path", p,
			"err", err.Error(),
		)
		return codexAuthFile{}, fmt.Errorf("read codex auth file: %w", err)
	}
	var doc codexAuthFile
	if err := json.Unmarshal(data, &doc); err != nil {
		log.Warn("adapter.codex.auth_file.parse_failed",
			"subcomponent", "adapter",
			"path", p,
			"body_bytes", len(data),
			"err", err.Error(),
		)
		return codexAuthFile{}, fmt.Errorf("parse codex auth file: %w", err)
	}
	return doc, nil
}

func (s *Server) readCodexAccessToken() (string, error) {
	doc, err := s.readCodexAuthFile()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(doc.Tokens.AccessToken) == "" {
		return "", errors.New("codex auth file missing tokens.access_token")
	}
	return doc.Tokens.AccessToken, nil
}

// codexAuthLookup adapts the Server's existing auth-file reader to
// the provider.AuthLookup interface so the Codex Provider can ask for
// a fresh token without depending on the daemon's internals.
type codexAuthLookup struct {
	server *Server
}

func (a codexAuthLookup) Token(_ context.Context) (string, error) {
	if a.server == nil {
		return "", errors.New("codex auth lookup: nil server")
	}
	return a.server.readCodexAccessToken()
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
