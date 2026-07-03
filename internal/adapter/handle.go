package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/gklog/correlation"
)

// adapterHandler is the canonical handler signature for adapter HTTP routes.
// Every route registers through Server.handle so the boundary owns
// correlation, panic recovery, and the body-not-written backstop. Returning
// an error routes through respondAdapterError; returning nil without writing
// anything triggers a synthesized catch-all error envelope.
type adapterHandler func(ctx context.Context, hctx *handlerCtx) error

// handlerCtx is the per-request value passed to adapterHandler. The Writer
// and Request fields hold the same plumbing the wrapper sees so handlers do
// not need to thread [http.ResponseWriter] and [*http.Request] separately.
type handlerCtx struct {
	Writer      http.ResponseWriter
	Request     *http.Request
	Family      adapterRouteFamily
	RequestID   string
	Correlation correlation.Context
}

// handle wraps an adapterHandler with the global adapter error boundary.
// The wrapper owns:
//   - request id assignment and correlation context propagation,
//   - livetrack ingress session registration so reload drain sees the
//     in-flight request; the session is released on handler return,
//   - 503 rejection when the ingress registry is draining (reload in
//     progress) so new requests are redirected to the replacement daemon,
//   - panic recovery (subsumes the legacy withAdapterErrorBoundary),
//   - body-not-written backstop: if fn returns nil but never wrote bytes,
//     a synthesized 502 catch-all envelope is emitted so a future code path
//     cannot silently exit without sending a response,
//   - dispatch of returned errors through respondAdapterError so error
//     envelope shape is unchanged on this branch.
//
// The returned [http.HandlerFunc] is what gets registered on the mux.
func (s *Server) handle(family adapterRouteFamily, fn adapterHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := strings.TrimSpace(r.Header.Get(clydeingress.HeaderRequestID))
		if reqID == "" {
			reqID = newRequestID()
		}
		corr := clydeingress.FromHTTPHeader(r.Header, reqID)
		if ingress := s.ingress; ingress != nil {
			ingressCtx := ingress.TranslateHeaders(r.Header)
			if ingressCtx.ConversationID != "" && clydeingress.ChatKey(corr) == "" {
				corr = clydeingress.WithChatIdentity(corr, ingressCtx.ConversationID, "native", ingressCtx.ConversationID, "")
			}
			corr = corr.WithIdentityAttributes(ingress.CorrelationAttrs(ingressCtx)...)
		}
		clydeingress.SetHTTPHeaders(corr, r.Header)
		clydeingress.SetHTTPHeaders(corr, w.Header())
		ctx, ingressCancel := context.WithCancel(correlation.WithContext(r.Context(), corr))
		defer ingressCancel()
		ctx = context.WithValue(ctx, ingressCancelKey{}, ingressCancel)
		r = r.WithContext(ctx)

		ingressSess, draining := s.acquireIngressSession(r, family, reqID)
		if draining {
			drainingErr := newAdapterError(adapterErrorUpstreamUnavailable, "service draining: reload in progress")
			drainingErr.HTTPStatus = http.StatusServiceUnavailable
			s.respondAdapterError(w, r, drainingErr)
			return
		}
		defer s.releaseIngressSession(ctx, ingressSess)

		rw := &adapterRecoveryWriter{ResponseWriter: w, wroteHeader: false}
		hctx := &handlerCtx{
			Writer:      rw,
			Request:     r,
			Family:      family,
			RequestID:   reqID,
			Correlation: corr,
		}
		s.dispatchHandler(ctx, w, r, rw, hctx, corr, family, fn)
	}
}

// acquireIngressSession builds IngressMeta from the request and registers
// a new session in the ingress registry. Returns (sess, false) on success,
// (nil, true) when the registry is draining and the caller should 503.
func (s *Server) acquireIngressSession(r *http.Request, family adapterRouteFamily, reqID string) (*livetrack.Session[IngressMeta], bool) {
	if s.requests == nil {
		return nil, false
	}
	meta := IngressMeta{
		RouteFamily:     string(family),
		RequestID:       reqID,
		CursorRequestID: strings.TrimSpace(r.Header.Get("X-Cursor-Request-Id")),
		Method:          r.Method,
		Path:            r.URL.Path,
		Stream:          false,
	}
	sess, ok := s.registerIngressSession(r, meta)
	if !ok {
		return nil, true
	}
	return sess, false
}

// releaseIngressSession releases the session when it is non-nil. Passing ctx
// from the handler goroutine threads correlation through the release event.
func (s *Server) releaseIngressSession(ctx context.Context, sess *livetrack.Session[IngressMeta]) {
	if sess == nil || s.requests == nil {
		return
	}
	s.requests.Release(ctx, sess, "adapter.ingress.completed")
}

// dispatchHandler executes fn and owns panic recovery, error routing, and
// the body-not-written backstop. It is extracted from handle to keep each
// closure under the funlen limit.
func (s *Server) dispatchHandler(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	rw *adapterRecoveryWriter,
	hctx *handlerCtx,
	corr correlation.Context,
	family adapterRouteFamily,
	fn adapterHandler,
) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		attrs := []slog.Attr{
			slog.Any("err", recovered),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route_family", string(family)),
			slog.String("remote_addr", r.RemoteAddr),
			slog.String("user_agent", r.UserAgent()),
			slog.String("stack", string(debug.Stack())),
			slog.Bool("response_started", rw.wroteHeader),
		}
		attrs = append(attrs, corr.Attrs()...)
		s.adapterErrorLog().LogAttrs(ctx, slog.LevelError, "adapter.request.panic", attrs...)
		if rw.wroteHeader {
			return
		}
		s.respondAdapterError(w, r, adapterErrInternal("adapter panic while handling request", fmt.Errorf("panic: %v", recovered)))
	}()

	err := fn(ctx, hctx)
	if err != nil {
		if rw.wroteHeader {
			// Headers were already committed (typical mid-stream
			// error path). The handler is responsible for any
			// in-band error frame. Log so a regression where the
			// handler swallows the error mid-stream is visible.
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route_family", string(family)),
				slog.String("err", err.Error()),
				slog.Bool("response_started", true),
			}
			attrs = append(attrs, corr.Attrs()...)
			s.adapterChatDispatchLog().LogAttrs(ctx, slog.LevelWarn, "adapter.request.error_after_headers", attrs...)
			return
		}
		s.respondAdapterError(w, r, err)
		return
	}

	// Body-not-written backstop. If the handler returns nil but
	// never wrote bytes the client would otherwise see an empty
	// 200. Synthesize a catch-all 502 envelope so the response
	// shape is always parsable.
	if !rw.wroteHeader {
		synth := newAdapterError(adapterErrorUpstreamFailed, "adapter handler returned without writing a response")
		synth.HTTPStatus = http.StatusBadGateway
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route_family", string(family)),
		}
		attrs = append(attrs, corr.Attrs()...)
		s.adapterChatDispatchLog().LogAttrs(ctx, slog.LevelError, "adapter.request.body_not_written", attrs...)
		s.respondAdapterError(w, r, synth)
	}
}
