package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
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

// StartOnListener serves the adapter on an already-bound listener.
// Daemon reload uses this to inherit the existing adapter socket
// without creating a bind gap.
func (s *Server) StartOnListener(ctx context.Context, lis net.Listener) error {
	s.connMu.Lock()
	s.conns = make(map[net.Conn]http.ConnState)
	s.connMu.Unlock()
	s.httpSrv = &http.Server{
		Addr:              lis.Addr().String(),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ConnState:         s.trackConnState,
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

// Shutdown stops accepting new adapter requests, closes idle keepalive
// connections, and lets active handlers finish until ctx expires.
// Cached upstream websocket sessions and in-flight outbound provider
// calls are drained through their respective livetrack registries so
// the reload deadline applies to egress traffic as well as ingress.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		if s.egressRegistry != nil {
			egressResult := s.egressRegistry.Drain(ctx, "adapter.shutdown")
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
	s.closeTrackedConns(http.StateIdle)
	err := s.httpSrv.Shutdown(ctx)
	if s.egressRegistry != nil {
		egressResult := s.egressRegistry.Drain(ctx, "adapter.shutdown")
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
func (s *Server) Close() error {
	if s.httpSrv == nil {
		return nil
	}
	s.closeTrackedConns(http.StateNew, http.StateActive, http.StateIdle, http.StateHijacked)
	return s.httpSrv.Close()
}

func (s *Server) trackConnState(conn net.Conn, state http.ConnState) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conns == nil {
		s.conns = make(map[net.Conn]http.ConnState)
	}
	if state == http.StateClosed {
		delete(s.conns, conn)
		return
	}
	s.conns[conn] = state
}

// ActiveRequestCount returns the number of HTTP connections currently
// serving a request (http.StateActive). Cloudflare-style keep-alive
// connections sit in StateIdle and are excluded so reload drain logic
// can distinguish "stream still streaming" from "tunnel is alive but
// nothing in flight". Concurrent-safe.
func (s *Server) ActiveRequestCount() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	n := 0
	for _, state := range s.conns {
		if state == http.StateActive {
			n++
		}
	}
	return n
}

// WaitForIdle polls ActiveRequestCount until it drops to zero or the
// supplied context is canceled. The polling cadence is fixed at 50ms
// which is well under the wall-time of any meaningful upstream call
// while keeping the busy loop bounded. Returns the final count when
// the context fires.
func (s *Server) WaitForIdle(ctx context.Context) int {
	if s.ActiveRequestCount() == 0 {
		return 0
	}
	t := time.NewTicker(50 * time.Millisecond)
	defer func() { t.Stop() }()
	for {
		select {
		case <-ctx.Done():
			return s.ActiveRequestCount()
		case <-t.C:
			if s.ActiveRequestCount() == 0 {
				return 0
			}
		}
	}
}

func (s *Server) closeTrackedConns(states ...http.ConnState) {
	if len(states) == 0 {
		return
	}
	wanted := make(map[http.ConnState]bool, len(states))
	for _, state := range states {
		wanted[state] = true
	}
	var toClose []net.Conn
	s.connMu.Lock()
	for conn, state := range s.conns {
		if wanted[state] {
			toClose = append(toClose, conn)
		}
	}
	s.connMu.Unlock()
	for _, conn := range toClose {
		_ = conn.Close()
	}
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
