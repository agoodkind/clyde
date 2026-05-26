package adapter

import (
	"context"
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
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		s.log.WarnContext(ctx, "adapter.listen_failed",
			"subcomponent", "adapter",
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

// StartOnListener serves the adapter on an already-bound listener.
// Daemon reload uses this to inherit the existing adapter socket
// without creating a bind gap.
func (s *Server) StartOnListener(ctx context.Context, lis net.Listener) error {
	s.httpSrv = &http.Server{
		Addr:              lis.Addr().String(),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(connCtx context.Context, c net.Conn) context.Context {
			return context.WithValue(connCtx, ingressConnKey{}, c)
		},
	}
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "adapter listening",
		slog.String("addr", lis.Addr().String()),
		slog.Int("models", len(s.registry.List())),
	)
	errCh := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.ErrorContext(ctx, "adapter.serve_panic",
					"subcomponent", "adapter",
					"addr", lis.Addr().String(),
					"err", fmt.Sprintf("panic: %v", recovered),
					"panic", recovered,
				)
				errCh <- fmt.Errorf("adapter serve panic: %v", recovered)
			}
		}()
		errCh <- s.httpSrv.Serve(lis)
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

// Shutdown stops accepting new adapter requests, drains in-flight
// ingress sessions and outbound provider calls until ctx expires, and
// closes the HTTP server. The paired registry.Drain call satisfies
// the livetrack adopter lint: any subsystem that calls
// [http.Server.Shutdown] must also call registry.Drain so the daemon
// reload chain has bounded shutdown. Cached upstream websocket
// sessions are also drained through their livetrack registry so the
// reload deadline applies to egress traffic as well as ingress.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: 0})
}

// ShutdownWith is the drain-option-aware sibling of [Server.Shutdown].
// The daemon reload chain calls it with [livetrack.DrainOptions.IdleGrace]
// set so the ingress and egress registries can force-close wedged
// sessions at drain start instead of waiting them out against the
// outer cap. Semantics and error handling are otherwise identical to
// [Server.Shutdown].
func (s *Server) ShutdownWith(ctx context.Context, opts livetrack.DrainOptions) error {
	if s.httpSrv == nil {
		if s.egressRegistry != nil {
			egressResult := s.egressRegistry.DrainWith(ctx, "adapter.shutdown", opts)
			s.log.InfoContext(ctx, "adapter.egress.drained",
				"final", egressResult.Final.String(),
				"remaining", egressResult.Remaining,
				"force_closed", egressResult.ForceClosed,
				"duration_ms", egressResult.Duration.Milliseconds(),
			)
		}
		if s.codexProvider != nil {
			s.codexProvider.DrainSessions(ctx, "adapter.shutdown")
		}
		return nil
	}
	s.httpSrv.SetKeepAlivesEnabled(false)
	if s.requests != nil {
		result := s.requests.DrainWith(ctx, "adapter.shutdown", opts)
		if len(result.Errors) > 0 {
			s.log.WarnContext(ctx, "adapter.ingress.drain_errors",
				"subcomponent", "adapter",
				"force_closed", result.ForceClosed,
				"errors", len(result.Errors),
			)
		}
	}
	err := s.httpSrv.Shutdown(ctx)
	if s.egressRegistry != nil {
		egressResult := s.egressRegistry.DrainWith(ctx, "adapter.shutdown", opts)
		s.log.InfoContext(ctx, "adapter.egress.drained",
			"final", egressResult.Final.String(),
			"remaining", egressResult.Remaining,
			"force_closed", egressResult.ForceClosed,
			"duration_ms", egressResult.Duration.Milliseconds(),
		)
	}
	if s.codexProvider != nil {
		s.codexProvider.DrainSessions(ctx, "adapter.shutdown")
	}
	return err
}

// Close force-closes all adapter HTTP connections. It is used after a
// bounded reload drain so stale keepalive or active Cloudflare
// connections cannot pin traffic to the old binary indefinitely.
// ForceCloseAll terminates every in-flight request session tracked in
// the ingress registry, then calls [http.Server.Close].
func (s *Server) Close() error {
	if s.httpSrv == nil {
		return nil
	}
	s.ForceCloseAll()
	return s.httpSrv.Close()
}

// ActiveRequestCount returns the number of in-flight requests currently
// tracked in the ingress registry. Cloudflare-style keep-alive
// connections that are idle between requests are not counted because
// the registry only holds sessions while a handler is executing.
// Concurrent-safe.
func (s *Server) ActiveRequestCount() int {
	if s.requests == nil {
		return 0
	}
	return s.requests.Count()
}

// WaitForIdle polls ActiveRequestCount until it drops to zero or the
// supplied context is canceled. The polling cadence is 50ms, matching
// the MITM registry poll so reload drain timing is symmetric across
// HTTP surfaces. Returns the final count when the context fires.
func (s *Server) WaitForIdle(ctx context.Context) int {
	if s.requests == nil || s.requests.Count() == 0 {
		return 0
	}
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return s.ActiveRequestCount()
		case <-t.C:
			if s.requests.Count() == 0 {
				return 0
			}
		}
	}
}

// ForceCloseAll closes every in-flight request session tracked in the
// ingress registry. The registry's force-close fan-out terminates each
// session's underlying TCP connection so wedged SSE streams, tool-call
// bridges, and synthetic-content marker round-trips do not outlive the
// drain deadline. Callers (daemon reload chain) invoke this after
// WaitForIdle returns non-zero.
func (s *Server) ForceCloseAll() {
	if s.requests == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	result := s.requests.Drain(ctx, "adapter.force_close_all")
	if len(result.Errors) > 0 {
		s.log.WarnContext(ctx, "adapter.ingress.force_close_errors",
			"subcomponent", "adapter",
			"force_closed", result.ForceClosed,
			"errors", len(result.Errors),
		)
	}
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
	mux.HandleFunc("/v1/completions", s.handle(adapterRouteOpenAI, s.auth(s.handleLegacy)))
	mux.HandleFunc("/v1/messages", s.handle(adapterRouteAnthropic, s.authAnthropic(s.handleAnthropicMessages)))
	mux.HandleFunc("/v1/messages/count_tokens", s.handle(adapterRouteAnthropic, s.authAnthropic(s.handleAnthropicCountTokens)))
	mux.HandleFunc("/", s.handle(adapterRouteHealth, s.handleRoot))
	return mux
}

func (s *Server) handleRoot(_ context.Context, hctx *handlerCtx) error {
	writeJSON(hctx.Writer, http.StatusOK, map[string]any{
		"service": "clyde-adapter",
		"paths":   []string{"/v1/models", "/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/messages/count_tokens", "/healthz"},
	})
	return nil
}

func (s *Server) handleHealth(_ context.Context, hctx *handlerCtx) error {
	writeJSON(hctx.Writer, http.StatusOK, map[string]string{"status": "ok"})
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
