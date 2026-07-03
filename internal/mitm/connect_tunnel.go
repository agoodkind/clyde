package mitm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/trace"
)

// handleConnect implements RFC 7230 section 4.3.6 HTTP CONNECT
// tunneling. Clients like codex-cli reach wss://chatgpt.com through
// the proxy by issuing CONNECT chatgpt.com:443 and then speaking
// TLS+websocket on the resulting stream. Without this handler the
// default mux returns 404 and the client cannot establish the
// upstream connection.
//
// Provider-owned CONNECT hosts terminate TLS inside Clyde with a
// generated leaf certificate, then route the decoded HTTP request
// through the provider capture path. Unclaimed CONNECT hosts keep
// opaque tunnel mode and forward bytes without payload capture.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	var spanErr error
	defer trace.Op(r.Context(), "mitm.connect.tunnel")(&spanErr)
	started := clock.Now()
	target := strings.TrimSpace(r.RequestURI)
	if target == "" {
		target = strings.TrimSpace(r.Host)
	}
	if target == "" {
		http.Error(w, "missing CONNECT target", http.StatusBadRequest)
		return
	}
	cleanTarget, connectHost, err := cleanConnectTarget(target)
	if err != nil {
		http.Error(w, "invalid CONNECT target: "+target, http.StatusBadRequest)
		return
	}
	if provider, claim, ok := providerForConnect(cleanTarget); ok {
		p.handleProviderTLSConnect(r.Context(), w, cleanTarget, claim.Host, provider, started)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	dialCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	upstream, err := new(net.Dialer).DialContext(dialCtx, "tcp", cleanTarget)
	if err != nil {
		p.log.Warn("mitm.connect.upstream_dial_failed", "concern", "providers.mitm.errors", "target", cleanTarget,
			"err", err,
		)
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		p.log.Warn("mitm.connect.hijack_failed", "concern", "providers.mitm.errors", "target", cleanTarget, "err", err)
		_ = upstream.Close()
		return
	}
	defer func() { _ = clientConn.Close() }()

	// Tell the client the tunnel is established. The client will
	// follow with TLS handshake + websocket frames.
	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.log.Warn("mitm.connect.write_established_failed", "concern", "providers.mitm.errors", "err", err)
		return
	}
	if err := bufrw.Flush(); err != nil {
		p.log.Warn("mitm.connect.flush_failed", "concern", "providers.mitm.errors", "err", err)
		return
	}

	closer := newTunnelCloser(&connCloser{conn: clientConn}, &connCloser{conn: upstream})
	sess, err := p.Tunnels.Register(r.Context(), "mitm.connect", TunnelMeta{
		ConnectHost:   connectHost,
		UpstreamAddr:  cleanTarget,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, closer)
	if err != nil {
		p.log.WarnContext(r.Context(), "mitm.connect.register_rejected", "concern", "providers.mitm.errors", "target", cleanTarget, "err", err)
		return
	}
	defer p.Tunnels.Release(r.Context(), sess, "mitm.connect.tunnel_closed")

	p.log.Info("mitm.connect.tunnel_open", "concern", "providers.mitm.wire", "target", cleanTarget,
		"host", connectHost,
	)
	bytesUp, bytesDown := spliceConnections(clientConn, upstream, sess)
	p.log.Info("mitm.connect.tunnel_closed", "concern", "providers.mitm.wire", "target", cleanTarget,
		"host", connectHost,
		"duration_ms", clock.Since(started).Milliseconds(),
		"bytes_up", bytesUp,
		"bytes_down", bytesDown,
	)
}

func cleanConnectTarget(target string) (string, string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(target))
	if err != nil {
		slog.Warn("mitm.connect.split_target_failed", "concern", "providers.mitm.errors", "target", target, "err", err)
		return "", "", fmt.Errorf("split CONNECT target: %w", err)
	}
	if strings.TrimSpace(host) == "" {
		return "", "", fmt.Errorf("missing CONNECT host")
	}
	if strings.ContainsRune(host, 0) || strings.ContainsRune(port, 0) {
		return "", "", fmt.Errorf("CONNECT target contains NUL")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		return "", "", fmt.Errorf("parse CONNECT port: %w", err)
	}
	if portNumber <= 0 || portNumber > 65535 {
		return "", "", fmt.Errorf("CONNECT port out of range")
	}
	return net.JoinHostPort(host, strconv.Itoa(portNumber)), host, nil
}

// spliceConnections forwards bytes between two connections in both
// directions until one side closes. Returns the count of bytes that
// flowed in each direction.
//
// session, when non-nil, has its activity timestamp refreshed after
// each successful write in either direction so the daemon reload-drain
// idle-grace fast-path can tell a wedged keepalive tunnel from an
// actively streaming one. Snapshot-copied sessions or callers that do
// not register with livetrack may pass nil; the Touch is a no-op.
func spliceConnections(client, upstream net.Conn, session *livetrack.Session[TunnelMeta]) (bytesUp, bytesDown int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	var upN, downN int64
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("mitm.connect.copy_up_panic", "concern", "providers.mitm.errors", "component", "mitm",
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		defer wg.Done()
		n, _ := io.Copy(&touchOnWrite{w: upstream, touch: session.Touch}, client)
		upN = n
		// Half-close so the upstream's read returns EOF instead of
		// hanging when the client closes its write side.
		if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("mitm.connect.copy_down_panic", "concern", "providers.mitm.errors", "component", "mitm",
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		defer wg.Done()
		n, _ := io.Copy(&touchOnWrite{w: client, touch: session.Touch}, upstream)
		downN = n
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
	return upN, downN
}

// touchOnWrite wraps an [io.Writer] so a livetrack session activity
// timestamp refreshes after each successful chunk write. The touch
// fires only when n > 0 so a zero-byte write does not falsely extend
// an idle session past the drain grace window. The wrapper exists so
// spliceConnections can stay inside [io.Copy] without losing the
// per-chunk activity signal that the idle-grace drain fast-path
// consumes.
type touchOnWrite struct {
	w     io.Writer
	touch func()
}

func (t *touchOnWrite) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if n > 0 && t.touch != nil {
		t.touch()
	}
	if err != nil {
		return n, fmt.Errorf("touch-on-write: %w", err)
	}
	return n, nil
}

func (p *Proxy) handleProviderTLSConnect(ctx context.Context, w http.ResponseWriter, target string, host string, provider Provider, started time.Time) {
	providerID := provider.ID().String()
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.hijack_failed", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "err", err)
		return
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.write_established_failed", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "err", err)
		return
	}
	if err := bufrw.Flush(); err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.flush_failed", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "err", err)
		return
	}

	ca, err := p.mitmCA()
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.ca_failed", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "err", err)
		return
	}
	leaf, err := ca.leafForHost(host)
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.leaf_failed", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "host", host, "err", err)
		return
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			p.log.DebugContext(ctx, "mitm.provider.connect.alpn_offer", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "host", host, "protos", info.SupportedProtos)
			nextProtos := mitmALPNProtocols(info.SupportedProtos)
			if len(nextProtos) == 0 {
				return nil, nil
			}
			return &tls.Config{
				Certificates: []tls.Certificate{*leaf},
				MinVersion:   tls.VersionTLS12,
				NextProtos:   nextProtos,
			}, nil
		},
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.client_tls_failed", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "host", host, "err", err)
		return
	}
	closer := newTunnelCloser(&connCloser{conn: tlsConn}, &connCloser{conn: clientConn})
	sess, err := p.Tunnels.Register(ctx, "mitm."+providerID+".tls", TunnelMeta{
		ConnectHost:   host,
		UpstreamAddr:  target,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, closer)
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.register_rejected", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "err", err)
		return
	}
	defer p.Tunnels.Release(ctx, sess, "mitm."+providerID+".tls.closed")
	p.log.InfoContext(ctx, "mitm.provider.connect.intercept_open", "concern", "providers.mitm.wire", "provider", providerID, "target", target, "host", host)
	switch tlsConn.ConnectionState().NegotiatedProtocol {
	case http2.NextProtoTLS:
		p.serveProviderInterceptedHTTP2(ctx, tlsConn, target, host, provider, sess)
	default:
		p.serveProviderInterceptedHTTP1(ctx, tlsConn, target, host, provider, sess)
	}
	p.log.InfoContext(ctx, "mitm.provider.connect.intercept_closed", "concern", "providers.mitm.wire", "provider", providerID,
		"target", target,
		"host", host,
		"duration_ms", clock.Since(started).Milliseconds(),
	)
}

func mitmALPNProtocols(offered []string) []string {
	for _, protocol := range offered {
		if protocol == http2.NextProtoTLS || protocol == "http/1.1" {
			return []string{http2.NextProtoTLS, "http/1.1"}
		}
	}
	return nil
}

func (p *Proxy) serveProviderInterceptedHTTP1(ctx context.Context, client *tls.Conn, target string, host string, provider Provider, parent *livetrack.Session[TunnelMeta]) {
	providerID := provider.ID().String()
	reader := bufio.NewReader(client)
	writer := bufio.NewWriter(client)
	sink := &bufioProviderResponseSink{proxy: p, bufw: writer}
	stopWatcher := make(chan struct{})
	var activeRequests atomic.Int32
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.log.WarnContext(ctx, "mitm.provider.tls.drain_watcher_panicked", "concern", "providers.mitm.wire", "provider", providerID,
					"host", host,
					"panic", recovered,
				)
			}
		}()
		p.watchProviderTunnelDrain(ctx, client, host, providerID, &activeRequests, stopWatcher)
	}()
	defer close(stopWatcher)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.DebugContext(ctx, "mitm.provider.http.read_request_failed", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", err)
			}
			return
		}
		// Reading a fresh request off the parent TLS session counts
		// as byte movement; refresh the parent's idle timestamp so
		// the daemon reload-drain idle-grace fast-path does not
		// evict a session that is actively serving requests through
		// a long-lived keepalive tunnel.
		parent.Touch()
		activeRequests.Add(1)
		// Register per-request session so the daemon's reload drain
		// sees in-flight cursor exchanges, not just the parent TLS
		// session. The closer terminates the underlying TLS conn so
		// force-close interrupts a hung intercepted request. The
		// parent linkage records the TLS session id so operators can
		// correlate request bursts back to their CONNECT tunnel.
		closer := newTunnelCloser(&connCloser{conn: client}, nil)
		reqSess, registerErr := p.Tunnels.Register(ctx, "mitm."+providerID+".http", TunnelMeta{
			ConnectHost:   host,
			UpstreamAddr:  target,
			CaptureFile:   "",
			KeepaliveSeen: false,
		}, closer, livetrack.WithParent(parent))
		if registerErr != nil {
			_ = req.Body.Close()
			if !errors.Is(registerErr, livetrack.ErrRegistryClosed) || parent == nil || p.Tunnels.State() != livetrack.StateDraining {
				activeRequests.Add(-1)
				p.log.WarnContext(ctx, "mitm.provider.http.register_rejected", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", registerErr)
				return
			}
			activeRequests.Add(-1)
			p.log.DebugContext(ctx, "mitm.provider.http.request_rejected_reload_drain", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", registerErr)
			return
		}
		if err := p.handleProviderInterceptedRequest(ctx, client, reader, sink, req, target, host, provider, parent); err != nil {
			activeRequests.Add(-1)
			p.log.WarnContext(ctx, "mitm.provider.http.request_failed", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "path", req.URL.Path, "err", err)
			if reqSess != nil {
				p.Tunnels.Release(ctx, reqSess, "mitm."+providerID+".http.failed")
			}
			return
		}
		// Response body forwarded successfully: refresh both the
		// parent TLS session and the per-request session so the
		// idle-grace drain fast-path sees activity on whichever
		// session a future reload chooses to inspect.
		parent.Touch()
		if reqSess != nil {
			reqSess.Touch()
		}
		activeRequests.Add(-1)
		if reqSess != nil {
			p.Tunnels.Release(ctx, reqSess, "mitm."+providerID+".http.completed")
		}
		if req.Close {
			return
		}
		if parent != nil && p.Tunnels.State() == livetrack.StateDraining {
			p.log.DebugContext(ctx, "mitm.provider.http.keepalive_closed_reload_drain", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "path", req.URL.Path)
			return
		}
	}
}

func (p *Proxy) watchProviderTunnelDrain(ctx context.Context, client *tls.Conn, host, providerID string, activeRequests *atomic.Int32, stop <-chan struct{}) {
	if p == nil || p.Tunnels == nil || client == nil {
		return
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if p.Tunnels.State() != livetrack.StateDraining {
				continue
			}
			if activeRequests != nil && activeRequests.Load() > 0 {
				continue
			}
			p.log.DebugContext(ctx, "mitm.provider.tls.closed_reload_drain", "concern", "providers.mitm.wire", "provider", providerID, "host", host)
			_ = client.Close()
			return
		}
	}
}

func buildProviderFailureInput(params providerForwardParams, requestIndex captureBodyIndex, responseIndex captureBodyIndex, statusCode int) httpCaptureRecordInput {
	return buildHTTPFailureCaptureInput(
		params.cfg,
		params.provider.ID().String(),
		"https://"+params.host+params.req.URL.RequestURI(),
		params.body,
		requestIndex,
		responseIndex,
		clock.Since(params.started),
		statusCode,
	)
}

func (p *Proxy) recordProviderFailure(req *http.Request, responseHeader http.Header, input httpCaptureRecordInput, failure httpFailureRecord) error {
	p.recordHTTPFailure(req, responseHeader, input, failure)
	return fmt.Errorf(failure.errorCode+": %s", failure.errorMessage)
}

type providerResponseSink interface {
	writeProviderResponse(resp *http.Response, bodyCap int) (bytesWritten int64, captured []byte, err error)
}

type bufioProviderResponseSink struct {
	proxy *Proxy
	bufw  *bufio.Writer
}

func (s *bufioProviderResponseSink) writeProviderResponse(resp *http.Response, bodyCap int) (int64, []byte, error) {
	return s.proxy.forwardAndCaptureProviderResponse(s.bufw, resp, bodyCap)
}

// handleProviderInterceptedRequest runs one intercepted provider request
// through the shared upstream and capture path. HTTP/2 callers pass nil for
// client and reader because h2 request bodies are already decoded by http2
// ServeConn and RFC 7540 does not use the HTTP/1.1 Upgrade websocket branch.
func (p *Proxy) handleProviderInterceptedRequest(ctx context.Context, client *tls.Conn, reader *bufio.Reader, writer providerResponseSink, req *http.Request, target string, host string, provider Provider, parent *livetrack.Session[TunnelMeta]) error {
	started := clock.Now()
	cfg := p.config()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return p.recordProviderFailure(req, http.Header{}, buildHTTPFailureCaptureInput(
			cfg,
			provider.ID().String(),
			"https://"+host+req.URL.RequestURI(),
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
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.RequestURI = ""
	req.URL.Scheme = "https"
	req.URL.Host = target
	if isWebsocketUpgrade(req) {
		bufioSink, ok := writer.(*bufioProviderResponseSink)
		if client == nil || reader == nil || !ok || bufioSink == nil {
			return fmt.Errorf("provider websocket requires intercepted TLS connection")
		}
		return p.handleProviderInterceptedWebsocket(ctx, client, reader, bufioSink.bufw, req, target, host, provider, parent)
	}

	concern := resolveCaptureConcern(cfg.CaptureRules, captureConcernInput{
		Provider:            provider.ID().String(),
		Host:                host,
		Method:              req.Method,
		Path:                req.URL.Path,
		RequestContentType:  req.Header.Get("Content-Type"),
		ResponseContentType: "",
	})
	params := providerForwardParams{
		writer:       writer,
		req:          req,
		body:         body,
		target:       target,
		host:         host,
		provider:     provider,
		started:      started,
		concern:      concern,
		requestBytes: int64(len(body)),
		cfg:          cfg,
	}

	return p.forwardProviderRequestToUpstream(ctx, params)
}

// providerForwardParams bundles the per-request state computed inside
// handleProviderInterceptedRequest so the forwarding helper can run the
// upstream round-trip and capture-metadata write without a wide
// parameter list.
type providerForwardParams struct {
	writer       providerResponseSink
	req          *http.Request
	body         []byte
	target       string
	host         string
	provider     Provider
	started      time.Time
	concern      string
	requestBytes int64
	cfg          config.MITMConfig
}

// forwardProviderRequestToUpstream runs the standard (non-hook)
// pass-through path: round-trip to upstream, stream the response to
// the client, and append capture metadata. Split out of
// handleProviderInterceptedRequest to keep both functions under the
// funlen ceiling.
func (p *Proxy) forwardProviderRequestToUpstream(ctx context.Context, params providerForwardParams) error {
	resp, err := p.providerUpstreamRoundTrip(params.req, params.body, params.target, params.host)
	if err != nil {
		return p.recordProviderFailure(params.req, http.Header{}, buildProviderFailureInput(params, emptyCaptureBodyIndex(), emptyCaptureBodyIndex(), http.StatusBadGateway), httpFailureRecord{
			includePayload:      true,
			includeUpstreamSend: true,
			errorCode:           "upstream_round_trip_failed",
			errorMessage:        err.Error(),
		})
	}
	defer func() { _ = resp.Body.Close() }()

	responseBytes, responseBody, err := params.writer.writeProviderResponse(resp, p.captureBodyCap(params.cfg))
	if err != nil {
		return err
	}
	p.log.InfoContext(ctx, "mitm.provider.capture.completed",
		"provider", params.provider.ID().String(),
		"host", params.host,
		"concern", params.concern,
		"path", params.req.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", clock.Since(params.started).Milliseconds(),
		"request_bytes", params.requestBytes,
		"response_bytes", responseBytes,
	)
	p.recordHTTPCapture(params.req, resp.Header, providerHTTPCaptureRecordInput(params, resp.StatusCode, responseBytes))
	p.recordProviderCaptureStore(params, resp.Header, resp.StatusCode, responseBody)
	p.diagnoseProviderExchange(ctx, params, "")
	p.recordProviderDriftCapture(ctx, params)
	return nil
}

// recordProviderCaptureStore persists an intercepted-TLS provider exchange to
// the shared SQLite capture store, tagged with this proxy's client id. The
// response body is the decoded buffer captured by
// [Proxy.forwardAndCaptureProviderResponse] up to the store cap. Identity and
// concern are derived from the same extraction the wire-leg emitter uses.
func (p *Proxy) recordProviderCaptureStore(params providerForwardParams, responseHeader http.Header, status int, responseBody []byte) {
	decodedBody, decoded := decodeForCapture(responseBody, responseHeader.Get("Content-Encoding"))
	if !decoded {
		decodedBody = responseBody
	}
	decodedRequest, requestDecoded := decodeForCapture(params.body, params.req.Header.Get("Content-Encoding"))
	if !requestDecoded {
		decodedRequest = params.body
	}
	concern := resolveCaptureConcern(params.cfg.CaptureRules, captureConcernInput{
		Provider:            params.provider.ID().String(),
		Host:                params.host,
		Method:              params.req.Method,
		Path:                params.req.URL.Path,
		RequestContentType:  params.req.Header.Get("Content-Type"),
		ResponseContentType: "",
	})
	contrib := extractIdentityContribution(params.host, params.req.URL.Path, params.req.Header)
	identity := mitmRequestIdentity(params.req.Header, contrib)
	p.store.Record(capture.Record{
		Timestamp:         clock.Now(),
		Client:            p.client,
		Provider:          params.provider.ID().String(),
		Concern:           concern,
		Host:              params.host,
		Method:            params.req.Method,
		Path:              params.req.URL.Path,
		Status:            status,
		RequestID:         identity.RequestID,
		UpstreamRequestID: identity.UpstreamRequestID,
		SessionID:         identity.SessionID,
		TraceID:           identity.TraceID,
		RequestHeaders:    params.req.Header,
		ResponseHeaders:   responseHeader,
		RequestBody:       decodedRequest,
		ResponseBody:      decodedBody,
		RequestType:       params.req.Header.Get("Content-Type"),
		ResponseType:      responseHeader.Get("Content-Type"),
		Duration:          clock.Since(params.started),
	})
}

// recordProviderDriftCapture feeds the provider TLS-intercept path's request
// into the drift-capture writer and debounces a baseline refresh, matching the
// plain-HTTP forward path. The intercepted request body may be content-encoded,
// so it is decoded before the body field-set summary and the claude billing
// attestation are derived. Native provider traffic reaches the upstream through
// this path, so the drift baseline learns from real client requests here.
func (p *Proxy) recordProviderDriftCapture(ctx context.Context, params providerForwardParams) {
	provider := params.provider.ID().String()
	decodedBody, decoded := decodeForCapture(params.body, params.req.Header.Get("Content-Encoding"))
	if !decoded {
		decodedBody = params.body
	}
	p.recordDriftCapture(params.cfg, driftCaptureInput{
		provider:    provider,
		method:      params.req.Method,
		path:        params.req.URL.Path,
		upstreamURL: "https://" + params.host + params.req.URL.RequestURI(),
		header:      params.req.Header,
		body:        decodedBody,
	})
	queueBaselineRefresh(ctx, p.store, params.cfg, provider, p.log)
}

// diagnoseProviderExchange invokes the claiming provider's optional
// [ExchangeDiagnostician] hook after the unified capture leg has been
// recorded. Providers that decode provider-specific diagnostics (only
// Cursor today) emit them on their own wire concern log; providers
// that do not implement the interface are skipped. The hook is given
// the decoded request body so a provider can inspect a gRPC-web frame
// without the generic layer knowing the wire shape.
func (p *Proxy) diagnoseProviderExchange(ctx context.Context, params providerForwardParams, hookName string) {
	diagnostician, ok := params.provider.(ExchangeDiagnostician)
	if !ok {
		return
	}
	decodedRequestBody, decoded := decodeForCapture(params.body, params.req.Header.Get("Content-Encoding"))
	if !decoded {
		decodedRequestBody = params.body
	}
	diagnostician.DiagnoseExchange(ctx, p.log, ExchangeDiagnostic{
		RequestHeader:      params.req.Header,
		DecodedRequestBody: decodedRequestBody,
		Method:             params.req.Method,
		Path:               params.req.URL.Path,
		Host:               params.host,
		Concern:            params.concern,
		HookName:           hookName,
	})
}

// providerHTTPCaptureRecordInput packages the provider request and the
// upstream response shape into the shared httpCaptureRecordInput used by
// recordHTTPCapture. Body persistence now lives in the SQLite capture store,
// so the wire-leg indexes carry the same filtered inline summaries the
// plain-HTTP path emits rather than raw-file references.
func providerHTTPCaptureRecordInput(params providerForwardParams, statusCode int, responseBytes int64) httpCaptureRecordInput {
	return httpCaptureRecordInput{
		config:         params.cfg,
		provider:       params.provider.ID().String(),
		upstreamURL:    "https://" + params.host + params.req.URL.RequestURI(),
		requestBody:    params.body,
		responseBody:   nil,
		requestIndex:   newCaptureBodyIndexFromSummary(summarizeBody(params.body)),
		responseIndex:  emptyCaptureBodyIndex(),
		responseLen:    responseBytes,
		duration:       clock.Since(params.started),
		responseStatus: statusCode,
		clientFacet:    nil,
	}
}

// providerUpstreamRoundTrip dials the upstream over the proxy's TLS
// client config and returns the response. Extracted from
// handleProviderInterceptedRequest to keep that function below the
// funlen threshold; the transport is constructed per-request and its
// idle connections are closed on return so this helper owns the
// transport's lifetime end-to-end.
func (p *Proxy) providerUpstreamRoundTrip(req *http.Request, body []byte, target string, host string) (*http.Response, error) {
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
		TLSClientConfig:     p.tlsClientConfig,
		DialContext:         p.dialContext,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(providerUpstreamRequest(req, body, target, host))
	if err != nil {
		p.log.Warn("mitm.provider.upstream_round_trip_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"host", host,
			"path", req.URL.Path,
			"err", err,
		)
		return nil, fmt.Errorf("cursor upstream round trip: %w", err)
	}
	return resp, nil
}

func providerUpstreamRequest(req *http.Request, body []byte, target string, host string) *http.Request {
	upstreamReq := req.Clone(req.Context())
	upstreamReq.Body = io.NopCloser(bytes.NewReader(body))
	upstreamReq.ContentLength = int64(len(body))
	upstreamReq.Host = host
	upstreamReq.URL.Scheme = "https"
	upstreamReq.URL.Host = target
	upstreamReq.RequestURI = ""
	upstreamReq.Header = req.Header.Clone()
	return upstreamReq
}

// forwardAndCaptureProviderResponse streams the upstream response back to the
// intercepted client and, in parallel, buffers the raw response body (up to
// bodyCap) so the completed exchange can be persisted to the SQLite capture
// store. It returns the total wire bytes written to the client and the
// buffered body bytes. Capture is best-effort: the body buffer caps silently
// and never affects what the client receives.
func (p *Proxy) forwardAndCaptureProviderResponse(client *bufio.Writer, resp *http.Response, bodyCap int) (int64, []byte, error) {
	bodyBuffer := &limitedBuffer{limit: bodyCap, buf: bytes.Buffer{}}
	chunked := resp.ContentLength < 0
	header := providerResponseHeader(resp, chunked)
	responseBytes, err := writeProviderResponseBytes(client, header, "header")
	if err != nil {
		return 0, bodyBuffer.Bytes(), err
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, _ = bodyBuffer.Write(buf[:n])
			written, err := writeProviderResponseBodyChunk(client, buf[:n], chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, bodyBuffer.Bytes(), err
			}
		}
		if errors.Is(readErr, io.EOF) {
			written, err := writeProviderResponseEOF(client, chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, bodyBuffer.Bytes(), err
			}
			return responseBytes, bodyBuffer.Bytes(), nil
		}
		if readErr != nil {
			p.log.Warn("mitm.provider.response.read_body_failed", "concern", "providers.mitm.wire", "err", readErr)
			return responseBytes, bodyBuffer.Bytes(), fmt.Errorf("read cursor response body: %w", readErr)
		}
	}
}

func providerResponseHeader(resp *http.Response, chunked bool) []byte {
	headers := resp.Header.Clone()
	headers.Del("Transfer-Encoding")
	if chunked {
		headers.Del("Content-Length")
		header := headerBlock(resp.Proto+" "+resp.Status+"\r\n", headers)
		return append(header[:len(header)-2], []byte("Transfer-Encoding: chunked\r\n\r\n")...)
	}
	headers.Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	return headerBlock(resp.Proto+" "+resp.Status+"\r\n", headers)
}

func writeProviderResponseBodyChunk(client *bufio.Writer, chunk []byte, chunked bool) (int64, error) {
	var written int64
	if chunked {
		chunkHeader := fmt.Appendf(nil, "%x\r\n", len(chunk))
		count, err := writeProviderResponseBytes(client, chunkHeader, "chunk header")
		written += count
		if err != nil {
			return written, err
		}
	}
	count, err := writeProviderResponseBytes(client, chunk, "body")
	written += count
	if err != nil {
		return written, err
	}
	if chunked {
		count, err = writeProviderResponseString(client, "\r\n", "chunk terminator")
		written += count
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func writeProviderResponseEOF(client *bufio.Writer, chunked bool) (int64, error) {
	if !chunked {
		if err := client.Flush(); err != nil {
			slog.Warn("mitm.provider.response.flush_body_failed", "concern", "providers.mitm.wire", "err", err)
			return 0, fmt.Errorf("flush cursor response body: %w", err)
		}
		return 0, nil
	}
	return writeProviderResponseString(client, "0\r\n\r\n", "chunk EOF")
}

func writeProviderResponseBytes(client *bufio.Writer, chunk []byte, label string) (int64, error) {
	if _, err := client.Write(chunk); err != nil {
		slog.Warn("mitm.provider.response.write_client_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("write cursor response %s: %w", label, err)
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.provider.response.flush_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("flush cursor response %s: %w", label, err)
	}
	return int64(len(chunk)), nil
}

func writeProviderResponseString(client *bufio.Writer, text string, label string) (int64, error) {
	if _, err := client.WriteString(text); err != nil {
		slog.Warn("mitm.provider.response.write_client_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("write cursor response %s: %w", label, err)
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.provider.response.flush_failed", "concern", "providers.mitm.wire", "label", label, "err", err)
		return 0, fmt.Errorf("flush cursor response %s: %w", label, err)
	}
	return int64(len(text)), nil
}
