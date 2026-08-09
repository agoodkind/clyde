package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"goodkind.io/clyde/internal/livetrack"
)

type h3ParentSessionContextKey struct{}

func h3ContextWithParent(ctx context.Context, parent *livetrack.Session[TunnelMeta]) context.Context {
	return context.WithValue(ctx, h3ParentSessionContextKey{}, parent)
}

func h3ParentFromContext(ctx context.Context) *livetrack.Session[TunnelMeta] {
	parent, _ := ctx.Value(h3ParentSessionContextKey{}).(*livetrack.Session[TunnelMeta])
	return parent
}

// ServeQUIC runs the proxy's HTTP/3 server on every bound UDP packet
// connection until ShutdownQUIC closes the server or a packet connection fails.
func (p *Proxy) ServeQUIC(ctx context.Context) error {
	if len(p.h3Conns) == 0 {
		return nil
	}
	p.log.InfoContext(ctx,
		"mitm.proxy.quic_started", "concern", "providers.mitm.lifecycle", "base_url", p.base,
		"capture_dir", p.cfg.CaptureDir,
		"providers", p.cfg.Providers,
		"client", p.client,
		"sockets", len(p.h3Conns),
	)
	errs := make(chan error, len(p.h3Conns))
	var wg sync.WaitGroup
	for _, conn := range p.h3Conns {
		wg.Go(func() {
			if err := p.serveProviderInterceptedHTTP3(ctx, conn); err != nil {
				errs <- err
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

func (p *Proxy) serveProviderInterceptedHTTP3(ctx context.Context, pc net.PacketConn) error {
	if pc == nil {
		return fmt.Errorf("mitm h3 packet conn is nil")
	}
	err := p.providerHTTP3Server(ctx).Serve(pc)
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	wrapped := fmt.Errorf("mitm h3 serve %s: %w", pc.LocalAddr().String(), err)
	p.log.ErrorContext(ctx, "mitm.proxy.quic_serve_failed", "concern", "providers.mitm.wire", "addr", pc.LocalAddr().String(), "err", wrapped)
	return wrapped
}

func (p *Proxy) providerHTTP3Server(ctx context.Context) *http3.Server {
	p.h3Mu.Lock()
	defer p.h3Mu.Unlock()
	if p.h3Server != nil {
		return p.h3Server
	}
	tlsConfig := http3.ConfigureTLSConfig(&tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			host := normalizeConnectHost(hello.ServerName)
			if host == "" {
				return nil, fmt.Errorf("http3 client hello missing server name")
			}
			ca, err := p.mitmCA()
			if err != nil {
				return nil, err
			}
			return ca.leafForHost(host)
		},
	})
	p.h3Server = &http3.Server{
		TLSConfig: tlsConfig,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p.handleProviderH3Stream(w, req)
		}),
		ConnContext: p.providerH3ConnContext(ctx),
		Logger:      p.log,
	}
	return p.h3Server
}

func (p *Proxy) providerH3ConnContext(baseCtx context.Context) func(context.Context, *quic.Conn) context.Context {
	return func(ctx context.Context, conn *quic.Conn) context.Context {
		host := normalizeConnectHost(conn.ConnectionState().TLS.ServerName)
		if host == "" {
			return ctx
		}
		provider := providerForHTTP3Host(host)
		target := net.JoinHostPort(host, "443")
		providerID := provider.ID().String()
		parentKind := providerTunnelKind(provider.ID(), "h3.conn")
		parent, err := p.Tunnels.Register(ctx, parentKind, TunnelMeta{
			ConnectHost:   host,
			UpstreamAddr:  target,
			CaptureFile:   "",
			KeepaliveSeen: false,
		}, &quicConnCloser{conn: conn})
		if err != nil {
			p.log.WarnContext(ctx, "mitm.provider.http3.register_rejected", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", err)
			_ = conn.CloseWithError(0, "service draining")
			return ctx
		}
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					p.log.WarnContext(ctx, "mitm.provider.http3.conn_release_panicked", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "panic", recovered)
				}
			}()
			<-conn.Context().Done()
			p.Tunnels.Release(context.WithoutCancel(baseCtx), parent, parentKind+".closed")
		}()
		return h3ContextWithParent(ctx, parent)
	}
}

func (p *Proxy) handleProviderH3Stream(w http.ResponseWriter, req *http.Request) {
	host := providerH3RequestHost(req)
	if host == "" {
		http.Error(w, "missing HTTP/3 host", http.StatusBadRequest)
		return
	}
	target := net.JoinHostPort(host, "443")
	provider := providerForHTTP3Host(host)
	providerID := provider.ID().String()
	// The request context is the incoming context for this handler. The
	// per-stream cancel derives from it; the livetrack registration uses a
	// cancel-detached copy so the session release still runs after the stream
	// context ends.
	reqCtx := req.Context()
	parent := h3ParentFromContext(reqCtx)
	streamCtx, cancelStream := context.WithCancel(reqCtx)
	defer cancelStream()
	req = req.WithContext(streamCtx)
	lifecycleCtx := context.WithoutCancel(reqCtx)
	closer := &h3StreamCloser{body: req.Body, cancel: cancelStream}
	streamKind := providerTunnelKind(provider.ID(), "h3")
	streamSession, registerErr := p.Tunnels.Register(lifecycleCtx, streamKind, TunnelMeta{
		ConnectHost:   host,
		UpstreamAddr:  target,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, closer, livetrack.WithParent(parent))
	if registerErr != nil {
		_ = req.Body.Close()
		if !errors.Is(registerErr, livetrack.ErrRegistryClosed) || parent == nil || p.Tunnels.State() != livetrack.StateDraining {
			p.log.WarnContext(lifecycleCtx, "mitm.provider.http3.stream_register_rejected", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", registerErr)
		} else {
			p.log.DebugContext(lifecycleCtx, "mitm.provider.http3.stream_rejected_reload_drain", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", registerErr)
		}
		http.Error(w, "service draining", http.StatusServiceUnavailable)
		return
	}
	releaseReason := streamKind + ".completed"
	defer func() {
		p.Tunnels.Release(lifecycleCtx, streamSession, releaseReason)
	}()

	sink := &h3ProviderResponseSink{rw: w, session: streamSession, wroteHeader: false}
	if err := p.handleProviderInterceptedRequest(streamCtx, nil, nil, sink, req, target, host, provider, parent, streamSession); err != nil {
		releaseReason = streamKind + ".failed"
		p.log.WarnContext(lifecycleCtx, "mitm.provider.http3.request_failed", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "path", req.URL.Path, "err", err)
		if !sink.wroteHeader {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
		return
	}
	if parent != nil {
		parent.Touch()
	}
	streamSession.Touch()
}

func providerH3RequestHost(req *http.Request) string {
	if req == nil {
		return ""
	}
	host := normalizeConnectHost(req.Host)
	if host != "" {
		return host
	}
	if req.TLS != nil {
		host = normalizeConnectHost(req.TLS.ServerName)
		if host != "" {
			return host
		}
	}
	return normalizeConnectHost(req.URL.Host)
}

func providerForHTTP3Host(host string) Provider {
	provider, _, ok := providerForConnect(net.JoinHostPort(host, "443"))
	if ok {
		return provider
	}
	return unknownProvider{}
}

func providerTunnelKind(providerID ProviderID, suffix string) string {
	if providerID == ProviderIDUnknown {
		return "mitm." + suffix
	}
	return "mitm." + providerID.String() + "." + suffix
}

type unknownProvider struct{}

func (unknownProvider) ID() ProviderID {
	return ProviderIDUnknown
}

func (unknownProvider) ClassifyConnect(string) ConnectClaim {
	return ConnectClaim{Claimed: false, Host: "", ProviderID: ProviderIDUnknown}
}

func (unknownProvider) ClassifyPlain(string) PlainRouteClaim {
	return PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}
}

func (unknownProvider) ExtractIdentity(http.Header) IdentityContribution {
	return IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
}

// quicConnResource adapts a *quic.Conn to the [io.Closer] shape. quic's
// teardown is CloseWithError rather than a plain Close, so this wrapper both
// makes the intent obvious and lets the lifecycle-closer analyzer recognize the
// QUIC connection as genuinely released.
type quicConnResource struct {
	conn   *quic.Conn
	reason string
}

func (q quicConnResource) Close() error {
	if err := q.conn.CloseWithError(0, q.reason); err != nil {
		wrapped := fmt.Errorf("close quic conn: %w", err)
		slog.Warn("mitm.quic.conn_close_failed", "concern", "providers.mitm.wire", "component", "mitm", "reason", q.reason, "err", wrapped)
		return wrapped
	}
	return nil
}

type quicConnCloser struct {
	conn *quic.Conn
}

func (c *quicConnCloser) Close(reason string) error {
	if c == nil || c.conn == nil {
		return nil
	}
	return quicConnResource{conn: c.conn, reason: reason}.Close()
}

type h3StreamCloser struct {
	body   io.Closer
	cancel context.CancelFunc
}

func (c *h3StreamCloser) Close(string) error {
	if c.cancel != nil {
		c.cancel()
	}
	if c.body == nil {
		return nil
	}
	if err := c.body.Close(); err != nil {
		wrapped := fmt.Errorf("close h3 stream body: %w", err)
		slog.Warn("mitm.provider.http3.stream_body_close_failed", "concern", "providers.mitm.wire", "component", "mitm", "err", wrapped)
		return wrapped
	}
	return nil
}

type h3ProviderResponseSink struct {
	rw          http.ResponseWriter
	session     *livetrack.Session[TunnelMeta]
	wroteHeader bool
}

func (s *h3ProviderResponseSink) writeProviderResponse(resp *http.Response, bodyCap int) (int64, []byte, error) {
	forwardResponseHeaders(s.rw.Header(), resp.Header)
	declareResponseTrailers(s.rw.Header(), resp.Trailer)
	s.wroteHeader = true
	s.rw.WriteHeader(resp.StatusCode)

	captureBuffer := &limitedBuffer{limit: bodyCap, buf: bytes.Buffer{}}
	counter := &countingWriter{writer: s.rw, bytesWritten: 0}
	flusher := &responseControllerFlusher{controller: http.NewResponseController(s.rw)}
	err := streamWithFlush(counter, captureBuffer, resp.Body, flusher, s.session)
	copyHeaders(s.rw.Header(), resp.Trailer)
	return counter.bytesWritten, captureBuffer.Bytes(), err
}

type responseControllerFlusher struct {
	controller *http.ResponseController
}

func (f *responseControllerFlusher) Flush() {
	if f == nil || f.controller == nil {
		return
	}
	_ = f.controller.Flush()
}

func declareResponseTrailers(header http.Header, trailer http.Header) {
	for key := range trailer {
		header.Add("Trailer", key)
	}
}

func (p *Proxy) providerHTTP3UpstreamRoundTrip(req *http.Request, target string, host string) (*http.Response, error) {
	resp, err := p.providerHTTP3Transport().RoundTrip(providerUpstreamRequest(req, target, host))
	if err != nil {
		p.log.Warn("mitm.provider.http3.upstream_round_trip_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"host", host,
			"path", req.URL.Path,
			"err", err,
		)
		return nil, fmt.Errorf("provider http3 upstream round trip: %w", err)
	}
	return resp, nil
}

func (p *Proxy) providerHTTP3Transport() *http3.Transport {
	p.h3Mu.Lock()
	defer p.h3Mu.Unlock()
	if p.h3Transport != nil {
		return p.h3Transport
	}
	p.h3Transport = &http3.Transport{
		TLSClientConfig:    p.tlsClientConfig,
		Dial:               p.dialProviderHTTP3,
		DisableCompression: true,
	}
	return p.h3Transport
}

func (p *Proxy) dialProviderHTTP3(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
	remoteAddr, err := p.resolveProviderHTTP3UDPAddr(ctx, addr)
	if err != nil {
		return nil, err
	}
	localConn, err := net.ListenUDP("udp", nil)
	if err != nil {
		wrapped := fmt.Errorf("listen h3 upstream udp: %w", err)
		p.log.WarnContext(ctx, "mitm.provider.http3.upstream_udp_listen_failed", "concern", "providers.mitm.wire", "addr", addr, "err", wrapped)
		return nil, wrapped
	}
	transport := &quic.Transport{Conn: localConn}
	conn, err := transport.DialEarly(ctx, remoteAddr, tlsCfg, cfg)
	if err != nil {
		_ = transport.Close()
		_ = localConn.Close()
		wrapped := fmt.Errorf("dial h3 upstream %s: %w", addr, err)
		p.log.WarnContext(ctx, "mitm.provider.http3.upstream_dial_failed", "concern", "providers.mitm.wire", "addr", addr, "err", wrapped)
		return nil, wrapped
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.log.WarnContext(ctx, "mitm.provider.http3.upstream_conn_cleanup_panicked", "concern", "providers.mitm.wire", "addr", addr, "panic", recovered)
			}
		}()
		<-conn.Context().Done()
		_ = transport.Close()
		_ = localConn.Close()
	}()
	return conn, nil
}

func (p *Proxy) resolveProviderHTTP3UDPAddr(ctx context.Context, addr string) (net.Addr, error) {
	if p.h3ResolveUDPAddr != nil {
		return p.h3ResolveUDPAddr(ctx, addr)
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		wrapped := fmt.Errorf("split h3 upstream addr %q: %w", addr, err)
		p.log.WarnContext(ctx, "mitm.provider.http3.upstream_addr_split_failed", "concern", "providers.mitm.wire", "addr", addr, "err", wrapped)
		return nil, wrapped
	}
	port, err := net.DefaultResolver.LookupPort(ctx, "udp", portText)
	if err != nil {
		wrapped := fmt.Errorf("lookup h3 upstream port %q: %w", portText, err)
		p.log.WarnContext(ctx, "mitm.provider.http3.upstream_port_lookup_failed", "concern", "providers.mitm.wire", "addr", addr, "err", wrapped)
		return nil, wrapped
	}
	ipAddrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		wrapped := fmt.Errorf("lookup h3 upstream host %q: %w", host, err)
		p.log.WarnContext(ctx, "mitm.provider.http3.upstream_host_lookup_failed", "concern", "providers.mitm.wire", "host", host, "err", wrapped)
		return nil, wrapped
	}
	for _, ipAddr := range ipAddrs {
		if ipAddr.IP != nil {
			return &net.UDPAddr{IP: ipAddr.IP, Port: port, Zone: ipAddr.Zone}, nil
		}
	}
	noAddr := fmt.Errorf("lookup h3 upstream host %q returned no addresses", host)
	p.log.WarnContext(ctx, "mitm.provider.http3.upstream_host_no_addrs", "concern", "providers.mitm.wire", "host", host, "err", noAddr)
	return nil, noAddr
}
