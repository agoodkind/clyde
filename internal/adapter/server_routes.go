package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// Start binds the TCP listener and serves until ctx is done.
func (s *Server) Start(ctx context.Context) error {
	addr := s.Addr()
	listenConfig := net.ListenConfig{}
	lis, err := listenConfig.Listen(ctx, "tcp", addr)
	if err != nil {
		s.log.WarnContext(ctx, "adapter.listen_failed", "concern", "adapter.http.errors", "subcomponent", "adapter",
			"addr", addr,
			"err", err.Error(),
		)
		return fmt.Errorf("adapter listen %s: %w", addr, err)
	}
	return s.StartOnListener(ctx, lis)
}

// ingressConnKey is the context key used to pass the accepted [net.Conn]
// into handler goroutines via [http.Server.ConnContext]. This lets the
// per-request ingress session register a closer that terminates the
// underlying TCP connection on force-close.
type ingressConnKey struct{}

// ingressLabelKey is the context key carrying the ingress label
// ("openai" or "cursor") derived from the listener a request arrived on.
type ingressLabelKey struct{}

// ingressLabelFromContext returns the ingress label stamped by
// [Server.StartOnListeners], or empty when the split is not configured.
func ingressLabelFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ingressLabelKey{}).(string); ok {
		return v
	}
	return ""
}

// ingressLabel maps an accepted connection to its ingress label by the
// local port it landed on: the configured Cursor ingress port is tagged
// "cursor", every other adapter port is tagged "openai".
func (s *Server) ingressLabel(c net.Conn) string {
	localPort := 0
	if tcpAddr, ok := c.LocalAddr().(*net.TCPAddr); ok {
		localPort = tcpAddr.Port
	}
	return ingressLabelForPort(s.cfg.CursorIngressPort, localPort)
}

// ingressLabelForPort returns "cursor" when localPort matches the configured
// Cursor ingress port (and that port is set), otherwise "openai".
func ingressLabelForPort(cursorPort, localPort int) string {
	if cursorPort > 0 && localPort == cursorPort {
		return "cursor"
	}
	return "openai"
}

// StartOnListener serves the adapter on a single already-bound listener.
// Daemon reload uses this to inherit the existing adapter socket without
// creating a bind gap.
func (s *Server) StartOnListener(ctx context.Context, lis net.Listener) error {
	return s.StartOnListeners(ctx, lis)
}

// StartOnListeners serves the adapter on one or more already-bound
// listeners that share the single HTTP server and mux. Each accepted
// connection is tagged with an ingress label by its local port, so the
// generic OpenAI-compatible port and the Cursor BYOK port are
// distinguishable for both request telemetry and streaming reasoning
// render mode selection.
func (s *Server) StartOnListeners(ctx context.Context, listeners ...net.Listener) error {
	if len(listeners) == 0 {
		return fmt.Errorf("adapter: no listeners to serve")
	}
	s.httpSrv = &http.Server{
		Addr:              listeners[0].Addr().String(),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(connCtx context.Context, c net.Conn) context.Context {
			connCtx = context.WithValue(connCtx, ingressConnKey{}, c)
			return context.WithValue(connCtx, ingressLabelKey{}, s.ingressLabel(c))
		},
	}
	addrs := make([]string, 0, len(listeners))
	for _, lis := range listeners {
		addrs = append(addrs, lis.Addr().String())
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter listening", slog.String("concern", "process.daemon.listeners"), slog.Any("addrs", addrs),
		slog.Int("models", len(s.modelRegistry().List())),
	)
	errCh := make(chan error, len(listeners))
	for _, lis := range listeners {
		go func(lis net.Listener) {
			defer func() {
				if recovered := recover(); recovered != nil {
					s.log.ErrorContext(ctx, "adapter.serve_panic", "concern", "adapter.http.errors", "subcomponent", "adapter",
						"addr", lis.Addr().String(),
						"err", fmt.Sprintf("panic: %v", recovered),
						"panic", recovered,
					)
					errCh <- fmt.Errorf("adapter serve panic: %v", recovered)
				}
			}()
			errCh <- s.httpSrv.Serve(lis)
		}(lis)
	}
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = s.ShutdownHTTP(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

// DisableKeepAlives stops the adapter HTTP server from keeping connections
// alive, so no new request rides an existing keepalive connection once reload
// drain begins. Nil-safe: a not-yet-serving server is a no-op. The daemon
// registers this as a PhaseIngress before-hook on the lifecycle group so it
// runs ahead of the ingress registry drain, reproducing the keepalives-off
// step the pre-refactor ShutdownWith ran first.
func (s *Server) DisableKeepAlives() {
	if s.httpSrv != nil {
		s.httpSrv.SetKeepAlivesEnabled(false)
	}
}

// ShutdownHTTP gracefully shuts down the adapter HTTP server, closing its
// listeners and waiting for non-hijacked handlers to return. The ingress,
// egress, and codex websocket registries drain as lifecycle-group members, so
// this hook covers only the HTTP server lifecycle. Nil-safe. The daemon
// registers it as a PhaseEgress before-hook so it runs after ingress sessions
// drain but before egress, matching the pre-refactor ShutdownWith ordering.
func (s *Server) ShutdownHTTP(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	if err := s.httpSrv.Shutdown(ctx); err != nil {
		s.log.WarnContext(ctx, "adapter.server.http_shutdown_failed", "concern", "adapter.http.ingress", "subcomponent", "adapter", "err", err)
		return fmt.Errorf("adapter HTTP shutdown: %w", err)
	}
	return nil
}

// registerIngressSession registers a new ingress request session in the
// livetrack registry. The closer is an ingressConnCloser backed by the
// [net.Conn] injected into the request context by ConnContext; when no
// conn is present ([httptest.NewRecorder] scenarios) the closer cancels
// the handler context created by handle.
//
// Returns nil, false when the registry has already begun draining
// (reload in progress): callers should write a 503 and return.
func (s *Server) registerIngressSession(r *http.Request, meta IngressMeta) (*livetrack.Session[IngressMeta], bool) {
	if s.requests == nil {
		return nil, true
	}
	var closer livetrack.Closer
	if conn, ok := r.Context().Value(ingressConnKey{}).(net.Conn); ok && conn != nil {
		closer = ingressConnCloser{conn: conn}
	} else if cancel, ok := r.Context().Value(ingressCancelKey{}).(context.CancelFunc); ok && cancel != nil {
		closer = &contextCancelCloser{cancel: cancel}
	} else {
		childCtx, cancel := context.WithCancel(r.Context())
		r = r.WithContext(childCtx)
		closer = &contextCancelCloser{cancel: cancel}
	}
	sess, err := s.requests.Register(r.Context(), "adapter.ingress", meta, closer)
	if err != nil {
		return nil, false
	}
	return sess, true
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handle(adapterRouteHealth, s.handleHealth))
	mux.HandleFunc("/v1/models", s.handle(adapterRouteOpenAI, s.auth(s.handleModels)))
	mux.HandleFunc("/v1/chat/completions", s.handle(adapterRouteOpenAI, s.auth(s.handleChat)))
	mux.HandleFunc("/v1/responses", s.handle(adapterRouteOpenAI, s.auth(s.handleResponses)))
	mux.HandleFunc("/v1/completions", s.handle(adapterRouteOpenAI, s.auth(s.handleLegacy)))
	mux.HandleFunc("/v1/messages", s.handle(adapterRouteAnthropic, s.authAnthropic(s.handleAnthropicMessages)))
	mux.HandleFunc("/v1/messages/count_tokens", s.handle(adapterRouteAnthropic, s.authAnthropic(s.handleAnthropicCountTokens)))
	mux.HandleFunc("/", s.handle(adapterRouteHealth, s.handleRoot))
	return mux
}

// rootRouteIndex is the typed payload the `/` handler returns to
// describe the adapter routes the daemon listens on.
type rootRouteIndex struct {
	Service string   `json:"service"`
	Paths   []string `json:"paths"`
}

// healthStatus is the typed payload the `/healthz` handler returns.
type healthStatus struct {
	Status string `json:"status"`
}

func (s *Server) handleRoot(ctx context.Context, hctx *handlerCtx) error {
	body, err := json.Marshal(rootRouteIndex{
		Service: "clyde-adapter",
		Paths:   []string{"/v1/models", "/v1/chat/completions", "/v1/responses", "/v1/completions", "/v1/messages", "/v1/messages/count_tokens", "/healthz"},
	})
	if err != nil {
		s.log.WarnContext(ctx, "adapter.root.marshal_failed", "concern", "adapter.http.errors", "err", err)
		return fmt.Errorf("marshal root index: %w", err)
	}
	writeJSON(hctx.Writer, body)
	return nil
}

func (s *Server) handleHealth(ctx context.Context, hctx *handlerCtx) error {
	body, err := json.Marshal(healthStatus{Status: "ok"})
	if err != nil {
		s.log.WarnContext(ctx, "adapter.health.marshal_failed", "concern", "adapter.http.errors", "err", err)
		return fmt.Errorf("marshal health response: %w", err)
	}
	writeJSON(hctx.Writer, body)
	return nil
}

// auth wraps an adapterHandler with bearer-token authentication for
// OpenAI-compat routes. When s.token is empty the wrapper is a passthrough.
// When the inbound Authorization header does not match, it returns an
// adapterErrorAuthFailed which the boundary shapes into the OpenAI error
// envelope.
func (s *Server) auth(next adapterHandler) adapterHandler {
	return func(ctx context.Context, hctx *handlerCtx) error {
		if s.token == "" {
			return next(ctx, hctx)
		}
		want := "Bearer " + s.token
		if hctx.Request.Header.Get("Authorization") != want {
			return newAdapterError(adapterErrorAuthFailed, "missing or invalid bearer token")
		}
		return next(ctx, hctx)
	}
}

// authAnthropic wraps an adapterHandler with bearer-token authentication for
// Anthropic native routes. Same behavior as auth; kept distinct so future
// Anthropic-specific auth (for example x-api-key) can land without churn.
func (s *Server) authAnthropic(next adapterHandler) adapterHandler {
	return func(ctx context.Context, hctx *handlerCtx) error {
		if s.token == "" {
			return next(ctx, hctx)
		}
		want := "Bearer " + s.token
		if hctx.Request.Header.Get("Authorization") != want {
			return newAdapterError(adapterErrorAuthFailed, "missing or invalid bearer token")
		}
		return next(ctx, hctx)
	}
}
