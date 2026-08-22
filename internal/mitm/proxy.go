package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/slogger"
)

// Proxy is part of Clyde's typed adapter surface.
type Proxy struct {
	log         *slog.Logger
	httpClient  *http.Client
	dialContext func(context.Context, string, string) (net.Conn, error)

	certMu sync.Mutex
	ca     *certAuthority

	tlsClientConfig *tls.Config
	requestLog      *logevent.Emitter

	// store is the shared SQLite capture sink that persists each completed
	// exchange. It may be nil (tests pass nil to skip capture); the capture
	// package's Record guards a nil receiver, so callers need not branch.
	store *capture.Store
	// client is the coarse client population label (the listener's ID) that
	// tags every capture.Record this proxy writes.
	client string

	// Tunnels tracks every long-lived MITM connection (CONNECT
	// tunnels, intercepted provider TLS sessions, plain HTTP request
	// loops) so the daemon can drain or force-close them on reload
	// instead of relying on [http.Server].Shutdown alone. See
	// internal/livetrack for the contract; the proxy installs each
	// session's closer to terminate hijacked client and upstream
	// connections.
	Tunnels *livetrack.Registry[TunnelMeta]

	mu                   sync.RWMutex
	cfg                  config.MITMConfig
	requestResponseHooks []RequestResponseHook
	// base is the loopback HTTP URL of the first bound listener, returned by
	// BaseURL for in-process clients (the adapter egress).
	base string
	// listeners holds every socket this proxy serves. A "localhost" listener id
	// binds two loopback sockets ([::1] and 127.0.0.1) served by this one proxy,
	// so every captured exchange across them carries the same client tag.
	listeners []net.Listener
	h3Conns   []net.PacketConn
	server    *http.Server
	h2Server  *http2.Server
	h3Server  *http3.Server

	h3Mu             sync.Mutex
	h3Transport      *http3.Transport
	h3ResolveUDPAddr func(context.Context, string) (net.Addr, error)
}

// NewProxy constructs a Proxy bound to the supplied listener. The
// caller (the daemon) owns listener lifecycle; the proxy serves on
// it until Shutdown returns. Callers must invoke Serve to start
// accepting requests.
//
// store is the shared SQLite capture sink every proxy records completed
// exchanges to; it may be nil to skip capture (the capture package guards a
// nil receiver). client is the coarse client population label (the listener's
// ID) that tags every record this proxy writes.
//
// The logging argument carries the typed request-emitter configuration
// (required-leg overrides and incomplete-policy) the MITM emitter applies. A
// zero-value [config.LoggingRequest] yields default required legs and the warn
// policy, matching the adapter's behavior.
func NewProxy(cfg config.MITMConfig, logging config.LoggingRequest, log *slog.Logger, listeners []net.Listener, h3Conns []net.PacketConn, store *capture.Store, client string, group *livetrack.Group) (*Proxy, error) {
	TunnelMeta{
		ConnectHost:   "",
		UpstreamAddr:  "",
		CaptureFile:   "",
		KeepaliveSeen: false,
	}.IsLivetrackMeta()
	if len(listeners) == 0 {
		return nil, fmt.Errorf("mitm: at least one listener is required")
	}
	for index, listener := range listeners {
		if listener == nil {
			return nil, fmt.Errorf("mitm: listener %d is nil", index)
		}
	}
	for index, conn := range h3Conns {
		if conn == nil {
			return nil, fmt.Errorf("mitm: h3 packet conn %d is nil", index)
		}
	}
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernProviderMITMLifecycle)
	ca, err := loadOrCreateCertAuthority(cfg.CA.CertPath, cfg.CA.KeyPath, time.Now)
	if err != nil {
		log.Warn(
			"mitm.tls.ca_load_failed", "concern", "providers.mitm.wire", "cert_path", cfg.CA.CertPath,
			"key_path", cfg.CA.KeyPath,
			"err", err,
		)
		return nil, fmt.Errorf("load mitm ca: %w", err)
	}
	p := &Proxy{
		log:             log.With("component", "mitm"),
		httpClient:      http.DefaultClient,
		dialContext:     (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:          sync.Mutex{},
		ca:              ca,
		tlsClientConfig: nil,
		requestLog: logevent.NewEmitter(
			slogger.WithConcern(log, slogger.ConcernProviderMITMWire),
			logevent.RequiredLegsFromStrings(logging.RequiredLegs),
			mitmEmitterOptions(logging)...,
		),
		store:  store,
		client: client,
		Tunnels: livetrack.Attach[TunnelMeta](group, livetrack.MemberSpec{
			Phase:         livetrack.PhaseIngress,
			QuietRelevant: true,
			CancelNoWait:  false,
		}, livetrack.Options[TunnelMeta]{
			Component:     "mitm",
			Concern:       slogger.ConcernProviderMITMLifecycle,
			Log:           log,
			PollEvery:     50 * time.Millisecond,
			CloserGrace:   2 * time.Second,
			ParallelClose: false,
			Now:           nil,
		}),
		mu:                   sync.RWMutex{},
		cfg:                  cfg,
		requestResponseHooks: nil,
		base:                 "http://" + listeners[0].Addr().String(),
		listeners:            listeners,
		h3Conns:              h3Conns,
		server:               nil,
		h2Server:             &http2.Server{},
		h3Server:             nil,

		h3Mu:             sync.Mutex{},
		h3Transport:      nil,
		h3ResolveUDPAddr: nil,
	}
	p.server = &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return p, nil
}

// mitmEmitterOptions converts the typed [config.LoggingRequest] into the
// [logevent.EmitterOption] set the MITM request emitter uses. Production
// callers wire a nil test-fail handler; tests wire a non-nil handler to fail
// the active test under [logevent.IncompletePolicyFailTest].
func mitmEmitterOptions(request config.LoggingRequest) []logevent.EmitterOption {
	policy, ok := logevent.ParseIncompletePolicy(request.IncompletePolicy)
	if !ok {
		policy = logevent.IncompletePolicyWarn
	}
	return []logevent.EmitterOption{
		logevent.WithIncompletePolicy(policy),
		logevent.WithTestingTB(nil),
	}
}

// Serve runs the proxy's HTTP server on every bound listener concurrently. It
// blocks until Shutdown is called or a listener returns an unrecoverable error,
// and returns the first non-[http.ErrServerClosed] error any listener produced.
// A single [http.Server] may serve multiple listeners; its Shutdown closes all
// of them, so the per-listener goroutines unblock together on drain.
func (p *Proxy) Serve(ctx context.Context) error {
	if len(p.listeners) == 0 {
		return fmt.Errorf("mitm: proxy has no listener")
	}
	p.log.InfoContext(
		ctx,
		"mitm.proxy.started", "concern", "providers.mitm.lifecycle", "base_url", p.base,
		"capture_dir", p.cfg.CaptureDir,
		"providers", p.cfg.Providers,
		"client", p.client,
		"sockets", len(p.listeners),
	)
	errs := make(chan error, len(p.listeners))
	var wg sync.WaitGroup
	for _, listener := range p.listeners {
		wg.Go(func() {
			if err := p.server.Serve(newSniffListener(ctx, listener, p)); err != nil && err != http.ErrServerClosed {
				p.log.ErrorContext(ctx, "mitm.proxy.serve_failed", "concern", "providers.mitm.wire", "addr", listener.Addr().String(), "err", err)
				errs <- fmt.Errorf("mitm serve %s: %w", listener.Addr().String(), err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ShutdownHTTP gracefully stops the proxy's HTTP server, which closes its
// listeners and unblocks the per-listener Serve goroutines. The CONNECT and
// per-request tunnels drain as lifecycle-group members (the Tunnels registry),
// so this hook covers only the HTTP server lifecycle. The daemon registers it
// as a PhaseIngress before-hook so the server stops accepting before the tunnel
// registry drains, reproducing the pre-refactor ShutdownWith ordering. The
// Cloudflare keepalive case (api2.cursor.sh CONNECT tunnels that never close on
// their own) is handled by the tunnel registry's idle-grace force-close, not
// here. Nil-safe and idempotent.
func (p *Proxy) ShutdownHTTP(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	if err := p.server.Shutdown(ctx); err != nil {
		p.log.WarnContext(ctx, "mitm.proxy.http_shutdown_failed", "concern", "providers.mitm.wire", "err", err)
		return fmt.Errorf("mitm shutdown: %w", err)
	}
	return nil
}

// ShutdownQUIC stops the HTTP/3 server and closes the UDP packet connections
// the daemon handed to this proxy. The upstream HTTP/3 transport is closed by a
// separate lifecycle hook after the tunnel registry drains, so in-flight
// streams can finish before their pooled upstream QUIC connections close.
func (p *Proxy) ShutdownQUIC(ctx context.Context) error {
	// providerHTTP3Server lazily initializes p.h3Server under h3Mu from the
	// ServeQUIC goroutines, so read the pointer under the same lock to avoid a
	// race with that first init, then release before the blocking Shutdown so
	// the lock never wraps a draining call.
	p.h3Mu.Lock()
	h3Server := p.h3Server
	p.h3Mu.Unlock()
	var closeErrs []error
	if h3Server != nil {
		if err := h3Server.Shutdown(ctx); err != nil {
			p.log.WarnContext(ctx, "mitm.proxy.quic_shutdown_failed", "concern", "providers.mitm.wire", "err", err)
			closeErrs = append(closeErrs, fmt.Errorf("shutdown h3 server: %w", err))
		}
	}
	for _, conn := range p.h3Conns {
		if conn == nil {
			continue
		}
		if err := conn.Close(); err != nil {
			p.log.WarnContext(ctx, "mitm.proxy.quic_packet_conn_close_failed", "concern", "providers.mitm.wire", "addr", conn.LocalAddr().String(), "err", err)
			closeErrs = append(closeErrs, fmt.Errorf("close h3 packet conn %s: %w", conn.LocalAddr().String(), err))
		}
	}
	return errors.Join(closeErrs...)
}

// CloseQUICTransport closes the shared upstream HTTP/3 transport after the
// proxy's livetrack sessions have drained.
func (p *Proxy) CloseQUICTransport() error {
	p.h3Mu.Lock()
	defer p.h3Mu.Unlock()
	if p.h3Transport == nil {
		return nil
	}
	if err := p.h3Transport.Close(); err != nil {
		wrapped := fmt.Errorf("close h3 upstream transport: %w", err)
		p.log.Warn("mitm.quic.upstream_transport_close_failed", "concern", "providers.mitm.wire", "component", "mitm", "err", wrapped)
		return wrapped
	}
	p.h3Transport = nil
	return nil
}

func (p *Proxy) config() config.MITMConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// ApplyConfig swaps the proxy's config-derived state (provider routing, capture
// settings) under the existing lock for an in-process config apply. Every
// request reads the config through config(), so a new request sees the swapped
// value while an established tunnel keeps the behavior it started with until it
// closes, matching the docs/cursor.md drain contract. Listener topology is never
// changed here (that routes to rebind), so no socket is touched.
func (p *Proxy) ApplyConfig(newCfg config.MITMConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = newCfg
	return nil
}

// defaultCaptureBodyCap is the fallback response-body buffer cap when the
// configured capture-store MaxBodyBytes is unset. It mirrors the capture
// package's own default so a body that the store will persist in full is not
// truncated by the proxy's response buffer first.
const defaultCaptureBodyCap = 8 * 1024 * 1024

// captureBodyCap returns the number of response bytes the proxy buffers for
// the capture store, defaulting to [defaultCaptureBodyCap] when the configured
// store MaxBodyBytes is non-positive.
func (p *Proxy) captureBodyCap(cfg config.MITMConfig) int {
	if cfg.CaptureStore.MaxBodyBytes > 0 {
		return cfg.CaptureStore.MaxBodyBytes
	}
	return defaultCaptureBodyCap
}

// upstreamRequest captures the inbound request fields the proxy
// forwards to the upstream HTTPS endpoint. Splitting the inbound
// [http.Request] from the upstream request lets the upstream call
// run on its own context (decoupled from r.Context() to survive
// stdlib [http.Server] lifecycle cancellations; CLYDE-324) without
// passing two distinct contexts into the same dispatch helper.
type upstreamRequest struct {
	method   string
	path     string
	header   http.Header
	body     []byte
	provider string
	upstream string
}

// dispatchUpstream constructs and executes the upstream request on
// upstreamCtx and returns the response. On failure it writes the
// error response to w and returns false. The upstreamCtx is supplied
// by [Proxy.registerPlainHTTP] and is NOT [http.Request.Context]: it
// shares request-scoped values via [context.WithoutCancel] but breaks
// the cancel chain, so stdlib [http.Server] lifecycle transitions and
// HTTP keep-alive churn do not abort the upstream stream mid-flight.
// Force-close from the registry's [mitmHTTPCloser] cancels upstreamCtx
// directly. Genuine client disconnect surfaces via streamWithFlush's
// failed write, which fires the deferred release in handle (CLYDE-324).
func (p *Proxy) dispatchUpstream(upstreamCtx context.Context, w http.ResponseWriter, req upstreamRequest) (*http.Response, bool) {
	upstreamURL := req.upstream + req.path
	upReq, err := http.NewRequestWithContext(upstreamCtx, req.method, upstreamURL, bytes.NewReader(req.body))
	if err != nil {
		http.Error(w, "build upstream request", http.StatusInternalServerError)
		return nil, false
	}
	copyHeaders(upReq.Header, req.header)
	upReq.Host = ""
	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		p.log.WarnContext(upstreamCtx, "mitm.proxy.upstream_failed", "concern", "providers.mitm.errors", "provider", req.provider, "path", req.path, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return nil, false
	}
	return resp, true
}

// registerPlainHTTP records the plain-HTTP exchange in the tunnel
// registry so reload drain sees in-flight requests. The caller owns
// the upstream context lifecycle and supplies cancel; the registered
// [mitmHTTPCloser] invokes cancel on force-close. Returns false (and
// writes a 503) when the registry has already begun draining.
//
// The caller derives ctx via [context.WithCancel] of
// [context.WithoutCancel] applied to [http.Request.Context] so that
// trace and other request-scoped values flow through to the upstream
// call but the cancel chain from r.Context() is broken. This is the
// CLYDE-324 fix: the inbound r.Context() is cancelled by the stdlib
// [http.Server] lifecycle on shutdown and by HTTP keep-alive churn,
// and that cancellation would silently abort the in-flight upstream
// HTTPS request to api.anthropic.com mid-stream. ctx is cancelled
// only by the registry's force-close path (via [mitmHTTPCloser.Close],
// which calls cancel) or by the deferred release at the end of the
// handler. Genuine client disconnect surfaces via streamWithFlush's
// failed write, which fires the deferred release naturally.
func (p *Proxy) registerPlainHTTP(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, r *http.Request, upstream string) (*livetrack.Session[TunnelMeta], func(string), bool) {
	reqSess, registerErr := p.Tunnels.Register(ctx, "mitm.http", TunnelMeta{
		ConnectHost:   r.Host,
		UpstreamAddr:  upstream,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, &mitmHTTPCloser{cancel: cancel})
	if registerErr != nil {
		cancel()
		p.log.WarnContext(ctx, "mitm.http.register_rejected", "concern", "providers.mitm.wire", "path", r.URL.Path, "err", registerErr)
		http.Error(w, "service draining", http.StatusServiceUnavailable)
		return nil, nil, false
	}
	release := func(reason string) {
		p.Tunnels.Release(ctx, reqSess, reason)
		cancel()
	}
	return reqSess, release, true
}

func (p *Proxy) recordPlainHTTPReadFailure(r *http.Request, cfg config.MITMConfig, provider, upstreamURL string, started time.Time, err error) {
	p.recordHTTPFailure(r, http.Header{}, buildHTTPFailureCaptureInput(
		cfg,
		provider,
		upstreamURL,
		nil,
		emptyCaptureBodyIndex(),
		emptyCaptureBodyIndex(),
		clock.Since(started),
		http.StatusBadRequest,
	), httpFailureRecord{
		includePayload:      false,
		includeUpstreamSend: false,
		errorCode:           "request_read_failed",
		errorMessage:        err.Error(),
	})
}

func (p *Proxy) recordPlainHTTPDispatchFailure(r *http.Request, cfg config.MITMConfig, provider, upstreamURL string, body []byte, requestIndex captureBodyIndex, started time.Time) {
	p.recordHTTPFailure(r, http.Header{}, buildHTTPFailureCaptureInput(
		cfg,
		provider,
		upstreamURL,
		body,
		requestIndex,
		emptyCaptureBodyIndex(),
		clock.Since(started),
		http.StatusBadGateway,
	), httpFailureRecord{
		includePayload:      true,
		includeUpstreamSend: true,
		errorCode:           "upstream_dispatch_failed",
		errorMessage:        "upstream dispatch failed",
	})
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	started := clock.Now()
	cfg := p.config()
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	provider, upstream := classifyRoute(r.URL.Path)
	if provider == "" {
		http.Error(w, "unsupported mitm route", http.StatusNotFound)
		return
	}
	if isWebsocketUpgrade(r) {
		p.handleWebsocket(w, r, provider, upstream)
		return
	}
	// CLYDE-324: upstream context shares request-scoped values with
	// r.Context() (trace ids, etc.) via [context.WithoutCancel] but
	// breaks the cancel chain so stdlib [http.Server] lifecycle changes
	// and HTTP keep-alive churn cannot abort the upstream HTTPS stream
	// mid-flight. Force-close from the registry cancels via the closer
	// installed in registerPlainHTTP; the deferred release also
	// cancels at handler return.
	upstreamCtx, upstreamCancel := context.WithCancel(context.WithoutCancel(r.Context()))
	plainHTTPSession, releasePlainHTTP, ok := p.registerPlainHTTP(upstreamCtx, upstreamCancel, w, r, upstream)
	if !ok {
		return
	}
	defer releasePlainHTTP("mitm.http.completed")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.recordPlainHTTPReadFailure(r, cfg, provider, upstream+r.URL.RequestURI(), started, err)
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	transformer, reqTransformer, err := p.matchRequestResponseHook(newRequestResponseHookRequest(provider, r.Host, r, newStaticRequestResponseHookBody(body)))
	if err != nil {
		// A hook is an optional enhancement, so a match failure must not abort the
		// client request; forward it with no transformer. matchRequestResponseHook
		// already logged the failure, so do not log it again here.
		transformer = nil
		reqTransformer = nil
	}
	// Apply any request-body rewrite before indexing, dispatching, and capturing so
	// all three see the forwarded (possibly trimmed) body. Fail-open leaves body as-is.
	body, _ = p.transformRequestBody(upstreamCtx, reqTransformer, body)
	requestBodyIndex := newCaptureBodyIndexFromSummary(summarizeBody(body))
	resp, ok := p.dispatchUpstream(upstreamCtx, w, upstreamRequest{
		method:   r.Method,
		path:     r.URL.RequestURI(),
		header:   r.Header,
		body:     body,
		provider: provider,
		upstream: upstream,
	})
	if !ok {
		p.recordPlainHTTPDispatchFailure(r, cfg, provider, upstream+r.URL.RequestURI(), body, requestBodyIndex, started)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	streamResp, err := p.applyResponseHook(upstreamCtx, transformer, resp)
	if err != nil {
		p.log.WarnContext(upstreamCtx, "mitm.proxy.response_hook_transform_failed", "concern", "providers.mitm.wire", "provider", provider, "path", r.URL.Path, "err", err)
		http.Error(w, "response hook failed", http.StatusBadGateway)
		return
	}
	// Close the transformed body only when the hook produced a new response; when
	// no transform ran applyResponseHook returns resp unchanged, so the defer
	// above already covers it and a second close would double-close.
	if streamResp != resp {
		defer func() { _ = streamResp.Body.Close() }()
	}

	forwardResponseHeaders(w.Header(), streamResp.Header)
	w.WriteHeader(streamResp.StatusCode)
	// Buffer the response up to the store body cap so the full body reaches
	// the capture store while still streaming to the client. The wire-leg
	// summary is derived from this same buffer (summarizeBody previews/caps).
	captureBuffer := &limitedBuffer{limit: p.captureBodyCap(cfg), buf: bytes.Buffer{}}
	flusher, _ := w.(http.Flusher)
	copyErr := streamWithFlush(w, captureBuffer, streamResp.Body, flusher, plainHTTPSession)
	duration := clock.Since(started)
	if copyErr != nil {
		p.log.Warn("mitm.proxy.copy_failed", "concern", "providers.mitm.wire", "provider", provider, "path", r.URL.Path, "err", copyErr)
	}
	p.finalizePlainHTTPCapture(r, streamResp, plainHTTPCaptureFinalize{
		cfg:              cfg,
		provider:         provider,
		upstream:         upstream,
		requestBody:      body,
		capture:          captureBuffer,
		requestBodyIndex: requestBodyIndex,
		duration:         duration,
	})
}

// plainHTTPCaptureFinalize bundles the post-stream state the plain-HTTP forward
// path threads into [Proxy.finalizePlainHTTPCapture]. Splitting the
// finalization out of [Proxy.handle] keeps that function under the funlen
// ceiling while preserving the capture / drift / baseline-refresh ordering.
type plainHTTPCaptureFinalize struct {
	cfg              config.MITMConfig
	provider         string
	upstream         string
	requestBody      []byte
	capture          *limitedBuffer
	requestBodyIndex captureBodyIndex
	duration         time.Duration
}

// finalizePlainHTTPCapture decodes the buffered response, records the unified
// HTTP capture leg, persists the exchange to the SQLite capture store, appends
// the drift-capture record for native upstreams, and debounces a baseline
// refresh.
func (p *Proxy) finalizePlainHTTPCapture(r *http.Request, resp *http.Response, fin plainHTTPCaptureFinalize) {
	captureBody, decoded := decodeForCapture(fin.capture.Bytes(), resp.Header.Get("Content-Encoding"))
	if decoded {
		p.log.Debug(
			"mitm.capture.decoded", "concern", "providers.mitm.wire", "provider", fin.provider,
			"path", r.URL.Path,
			"encoding", resp.Header.Get("Content-Encoding"),
			"raw_bytes", len(fin.capture.Bytes()),
			"decoded_bytes", len(captureBody),
		)
	}
	responseBodyIndex := newCaptureBodyIndexFromSummary(summarizeBody(captureBody))
	responseBodyLen := int64(len(captureBody))

	p.log.Info(
		"mitm.capture.completed", "concern", "providers.mitm.wire", "provider", fin.provider,
		"path", r.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", fin.duration.Milliseconds(),
	)
	upstreamURL := fin.upstream + r.URL.RequestURI()
	p.recordHTTPCapture(r, resp.Header, httpCaptureRecordInput{
		config:         fin.cfg,
		provider:       fin.provider,
		upstreamURL:    upstreamURL,
		requestBody:    fin.requestBody,
		responseBody:   captureBody,
		requestIndex:   fin.requestBodyIndex,
		responseIndex:  responseBodyIndex,
		responseLen:    responseBodyLen,
		duration:       fin.duration,
		responseStatus: resp.StatusCode,
		clientFacet:    nil,
	})
	p.recordCaptureStore(r, resp.Header, captureStoreInput{
		provider:        fin.provider,
		host:            r.Host,
		method:          r.Method,
		path:            r.URL.Path,
		status:          resp.StatusCode,
		requestBody:     fin.requestBody,
		responseBody:    captureBody,
		duration:        fin.duration,
		captureRules:    fin.cfg.CaptureRules,
		hasCaptureRules: true,
	})
	p.recordDriftCapture(fin.cfg, driftCaptureInput{
		provider:    fin.provider,
		method:      r.Method,
		path:        r.URL.Path,
		upstreamURL: upstreamURL,
		header:      r.Header,
		body:        fin.requestBody,
	})
	queueBaselineRefresh(r.Context(), p.store, fin.cfg, fin.provider, p.log)
}

// captureStoreInput bundles the typed fields needed to build a
// [capture.Record]. The proxy derives identity (request id, upstream request
// id, session id, trace id) from the same provider identity extraction the
// wire-leg emitter uses so a stored row joins back to its wire log.
type captureStoreInput struct {
	provider        string
	host            string
	method          string
	path            string
	status          int
	requestBody     []byte
	responseBody    []byte
	duration        time.Duration
	captureRules    []config.MITMCaptureRouteRule
	hasCaptureRules bool
}

// recordCaptureStore builds a [capture.Record] from the completed exchange and
// hands it to the shared store. The store's Record is non-blocking and
// nil-receiver-safe, so this fires unconditionally; a nil store is a no-op.
func (p *Proxy) recordCaptureStore(r *http.Request, responseHeader http.Header, in captureStoreInput) {
	contrib := extractIdentityContribution(r.Host, r.URL.Path, r.Header)
	identity := mitmRequestIdentity(r.Header, contrib)
	captureRules := in.captureRules
	if !in.hasCaptureRules {
		captureRules = p.config().CaptureRules
	}
	concern := resolveCaptureConcern(captureRules, captureConcernInput{
		Provider:            in.provider,
		Host:                in.host,
		Method:              in.method,
		Path:                in.path,
		RequestContentType:  r.Header.Get("Content-Type"),
		ResponseContentType: responseHeader.Get("Content-Type"),
	})
	// Headers win. Only when the provider sent no conversation header does the
	// body get consulted, and only for providers that carry the id in the body.
	if strings.TrimSpace(contrib.ConversationID) == "" {
		if identifier, ok := bodyConversationIdentifierFor(in.provider); ok {
			nativeID, found := identifier.ConversationIDFromBody(ExchangeDiagnostic{
				RequestHeader:      r.Header,
				DecodedRequestBody: in.requestBody,
				Method:             in.method,
				Path:               in.path,
				Host:               in.host,
				Concern:            concern,
				HookName:           "",
			})
			if found {
				contrib.ConversationID = nativeID
				contrib.ConversationSource = "body"
			}
		}
	}
	conversationID, conversationSource := captureConversationFields(in.provider, contrib)
	var decodedRequest *capture.DecodedBody
	if decoder, ok := captureDecoderFor(in.provider, in.host); ok {
		decoded, supported := decoder.DecodeCaptureRequest(ExchangeDiagnostic{
			RequestHeader:      r.Header,
			DecodedRequestBody: in.requestBody,
			Method:             in.method,
			Path:               in.path,
			Host:               in.host,
			Concern:            concern,
			HookName:           "",
		})
		if supported {
			decodedRequest = &decoded
		}
	}
	p.store.Record(capture.Record{
		Timestamp:          clock.Now(),
		Client:             p.client,
		Provider:           in.provider,
		Concern:            concern,
		Host:               in.host,
		Method:             in.method,
		Path:               in.path,
		Status:             in.status,
		RequestID:          identity.RequestID,
		UpstreamRequestID:  identity.UpstreamRequestID,
		SessionID:          identity.SessionID,
		ConversationID:     conversationID,
		ConversationSource: conversationSource,
		TraceID:            identity.TraceID,
		RequestHeaders:     r.Header,
		ResponseHeaders:    responseHeader,
		RequestBody:        in.requestBody,
		ResponseBody:       in.responseBody,
		RequestType:        r.Header.Get("Content-Type"),
		ResponseType:       responseHeader.Get("Content-Type"),
		DecodedRequest:     decodedRequest,
		Duration:           in.duration,
	})
}

// hopByHopHeader enumerates the response headers the MITM proxy
// strips before forwarding because they describe the upstream
// transport rather than the client-visible response body.
type hopByHopHeader string

const (
	hopByHopContentLength    hopByHopHeader = "content-length"
	hopByHopTransferEncoding hopByHopHeader = "transfer-encoding"
	hopByHopConnection       hopByHopHeader = "connection"
	hopByHopKeepAlive        hopByHopHeader = "keep-alive"
	hopByHopProxyConnection  hopByHopHeader = "proxy-connection"
	hopByHopUpgrade          hopByHopHeader = "upgrade"
	hopByHopTE               hopByHopHeader = "te"
	hopByHopTrailer          hopByHopHeader = "trailer"
)

func forwardResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		switch hopByHopHeader(strings.ToLower(key)) {
		case hopByHopContentLength, hopByHopTransferEncoding, hopByHopConnection,
			hopByHopKeepAlive, hopByHopProxyConnection, hopByHopUpgrade, hopByHopTE, hopByHopTrailer:
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type httpCaptureRecordInput struct {
	config         config.MITMConfig
	provider       string
	upstreamURL    string
	requestBody    []byte
	responseBody   []byte
	requestIndex   captureBodyIndex
	responseIndex  captureBodyIndex
	responseLen    int64
	duration       time.Duration
	responseStatus int
	// clientFacet is the typed provider-owned identity facet
	// produced by the registered MITM provider that claimed this
	// request. The MITM emit path attaches the facet to each event
	// without naming the provider.
	clientFacet logevent.Facet
}

type captureBodyIndex struct {
	raw json.RawMessage
}

func emptyCaptureBodyIndex() captureBodyIndex {
	return captureBodyIndex{raw: nil}
}

type captureBodySummary struct {
	Mode     string   `json:"mode,omitempty"`
	BodyType string   `json:"body_type,omitempty"`
	Bytes    int      `json:"bytes,omitempty"`
	SHA256   string   `json:"sha256,omitempty"`
	Keys     []string `json:"keys,omitempty"`
	Messages int      `json:"messages,omitempty"`
	Input    int      `json:"input,omitempty"`
	Tools    int      `json:"tools,omitempty"`
	Model    string   `json:"model,omitempty"`
	ArrayLen int      `json:"array_len,omitempty"`
}

func newCaptureBodyIndexFromSummary(summary captureBodySummary) captureBodyIndex {
	raw, err := json.Marshal(summary)
	if err != nil {
		raw = json.RawMessage(`null`)
	}
	return captureBodyIndex{raw: raw}
}

func (p *Proxy) recordHTTPCapture(r *http.Request, responseHeader http.Header, input httpCaptureRecordInput) {
	recorder := p.beginHTTPLogRecorder(r, &input)
	ctx := r.Context()
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMIngress, logevent.PhaseStarted, input)
	p.emitHTTPPayloadLeg(ctx, recorder, r, responseHeader, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMUpstreamSend, logevent.PhaseStarted, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMUpstreamStart, logevent.PhaseCompleted, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMForward, logevent.PhaseCompleted, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMCaptureIndex, logevent.PhaseCompleted, input)
	p.completeHTTPLogRecorder(ctx, recorder, input)
}

// classifyRoute dispatches plain-HTTP MITM routing to the registered
// provider that claims the supplied path. The generic MITM proxy
// never names a provider; provider packages declare their upstream
// claims via [RegisterProvider] at init time.
func classifyRoute(path string) (provider string, upstream string) {
	if _, claim, ok := providerForPlain(path); ok {
		return claim.Provider, claim.UpstreamURL
	}
	return "", ""
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func bodyTypeForCapture(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "empty"
	}
	switch trimmed[0] {
	case '{':
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return "json_object"
		}
	case '[':
		var decoded []json.RawMessage
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return "json_array"
		}
	}
	var scalar json.RawMessage
	if err := json.Unmarshal(trimmed, &scalar); err != nil {
		return "bytes"
	}
	return "json_scalar"
}

func bodyKeysForCapture(body []byte) []string {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &decoded); err != nil {
		return nil
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func summarizeBody(body []byte) captureBodySummary {
	summary := captureBodySummary{
		Mode:     "filtered_inline",
		BodyType: bodyTypeForCapture(body),
		Bytes:    len(body),
		SHA256:   sha256Hex(body),
		Keys:     nil,
		Messages: 0,
		Input:    0,
		Tools:    0,
		Model:    "",
		ArrayLen: 0,
	}
	return summarizeJSON(body, summary)
}

func summarizeJSON(body []byte, summary captureBodySummary) captureBodySummary {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return summary
	}
	if summary.BodyType == "bytes" {
		// Non-JSON bodies carry no field set to summarize, and the raw bytes are
		// never inlined into a summary; the full body lives only in the SQLite
		// capture store.
		return summary
	}
	if summary.BodyType == "json_array" {
		var values []json.RawMessage
		if err := json.Unmarshal(body, &values); err == nil {
			summary.ArrayLen = len(values)
		}
		return summary
	}
	if summary.BodyType != "json_object" {
		return summary
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return summary
	}
	summary.Keys = bodyKeysForCapture(body)
	summary.Messages = rawFieldArrayLen(fields, "messages")
	summary.Input = rawFieldArrayLen(fields, "input")
	summary.Tools = rawFieldArrayLen(fields, "tools")
	summary.Model = rawFieldString(fields, "model")
	if features, err := ExtractRequestFeatures(CapturedRequest{
		RequestHeaders: nil,
		RequestBody:    json.RawMessage(body),
	}); err == nil {
		summary.Model = features.ModelID
	}
	return summary
}

func rawFieldArrayLen(fields map[string]json.RawMessage, key string) int {
	raw, ok := fields[key]
	if !ok {
		return 0
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return 0
	}
	return len(values)
}

func rawFieldString(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

type limitedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remain := b.limit - b.buf.Len()
	if remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = b.buf.Write(p)
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

// streamWithFlush copies upstream response bytes to the client and
// the capture buffer in chunks, flushing after each successful read
// so SSE deltas reach the client in real time. Without the per-read
// flush, Go's [http.Server] buffers up to its internal threshold and
// stream consumers (claude-cli, Cursor) see batched deltas or hang
// waiting for the first byte.
//
// session, when non-nil, has its activity timestamp refreshed after
// each successful client write so the daemon reload-drain idle-grace
// fast-path can distinguish actively streaming SSE responses from
// wedged keepalive sessions. The session is the per-request livetrack
// handle returned by registerPlainHTTP; callers from outside the
// plain-HTTP path (tests, refactors) may pass nil.
func streamWithFlush(client io.Writer, capture io.Writer, src io.Reader, flusher http.Flusher, session *livetrack.Session[TunnelMeta]) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := client.Write(chunk); werr != nil {
				slog.Warn("mitm.stream.client_write_failed", "concern", "providers.mitm.wire", "err", werr)
				return fmt.Errorf("stream client write: %w", werr)
			}
			_, _ = capture.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
			if session != nil {
				session.Touch()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("stream upstream read: %w", err)
		}
	}
}
