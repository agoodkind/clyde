package mitm

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"goodkind.io/clyde/internal/clock"
)

const (
	transparentTLSHandshakeRecord byte = 0x16
	transparentSniffTimeout            = 30 * time.Second
)

type prefixConn struct {
	net.Conn
	prefix []byte
}

type closeWriter interface {
	CloseWrite() error
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	n, err := c.Conn.Read(b)
	switch {
	case n > 0 && errors.Is(err, io.EOF):
		// Deliver the bytes now with a nil error per the io.Reader contract; the
		// next Read returns (0, io.EOF). Returning (n>0, io.EOF) makes net/http
		// and crypto/tls treat the stream as closed while bytes are still unread.
		return n, nil
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		return n, io.EOF
	default:
		return n, fmt.Errorf("prefix conn read: %w", err)
	}
}

func (c *prefixConn) CloseWrite() error {
	closeWrite, ok := c.Conn.(closeWriter)
	if !ok {
		return nil
	}
	if err := closeWrite.CloseWrite(); err != nil {
		wrapped := fmt.Errorf("prefix conn close write: %w", err)
		slog.Warn("mitm.transparent.prefix_close_write_failed", "concern", "providers.mitm.wire", "component", "mitm", "err", wrapped)
		return wrapped
	}
	return nil
}

type sniffListener struct {
	net.Listener
	proxy     *Proxy
	conns     chan net.Conn
	acceptErr chan error
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newSniffListener(ctx context.Context, listener net.Listener, proxy *Proxy) *sniffListener {
	sniffer := &sniffListener{
		Listener:  listener,
		proxy:     proxy,
		conns:     make(chan net.Conn),
		acceptErr: make(chan error, 1),
		done:      make(chan struct{}),
		closeOnce: sync.Once{},
		closeErr:  nil,
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicErr := fmt.Errorf("transparent sniff accept loop panic: %v", recovered)
				proxy.log.WarnContext(ctx, "mitm.transparent.accept_loop_panicked", "concern", "providers.mitm.wire", "panic", recovered, "err", panicErr)
				// Close done and the listener so a caller blocked in Accept does
				// not hang forever after the accept loop dies.
				sniffer.fail(panicErr)
			}
		}()
		sniffer.acceptLoop(ctx)
	}()
	return sniffer
}

func (l *sniffListener) Accept() (net.Conn, error) {
	select {
	case err := <-l.acceptErr:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case conn := <-l.conns:
		return conn, nil
	case err := <-l.acceptErr:
		return nil, err
	case <-l.done:
		select {
		case err := <-l.acceptErr:
			return nil, err
		default:
			return nil, net.ErrClosed
		}
	}
}

func (l *sniffListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
		l.closeErr = l.Listener.Close()
	})
	if l.closeErr != nil {
		wrapped := fmt.Errorf("close sniff listener: %w", l.closeErr)
		l.proxy.log.Warn("mitm.transparent.listener_close_failed", "concern", "providers.mitm.wire", "component", "mitm", "err", wrapped)
		return wrapped
	}
	return nil
}

func (l *sniffListener) acceptLoop(ctx context.Context) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			if l.closed() || errors.Is(err, net.ErrClosed) {
				return
			}
			l.fail(fmt.Errorf("transparent sniff accept: %w", err))
			return
		}
		go func(rawConn net.Conn) {
			defer func() {
				if recovered := recover(); recovered != nil {
					l.proxy.log.WarnContext(ctx, "mitm.transparent.conn_sniff_panicked", "concern", "providers.mitm.wire", "panic", recovered)
					_ = rawConn.Close()
				}
			}()
			l.handleAcceptedConn(ctx, rawConn)
		}(conn)
	}
}

func (l *sniffListener) handleAcceptedConn(ctx context.Context, conn net.Conn) {
	if err := conn.SetReadDeadline(clock.Now().Add(transparentSniffTimeout)); err != nil {
		l.proxy.log.DebugContext(ctx, "mitm.transparent.set_sniff_deadline_failed", "concern", "providers.mitm.wire", "err", err)
		_ = conn.Close()
		return
	}
	var firstByte [1]byte
	n, err := conn.Read(firstByte[:])
	if resetErr := conn.SetReadDeadline(time.Time{}); resetErr != nil {
		l.proxy.log.DebugContext(ctx, "mitm.transparent.clear_sniff_deadline_failed", "concern", "providers.mitm.wire", "err", resetErr)
		_ = conn.Close()
		return
	}
	if n == 0 {
		if err != nil {
			l.proxy.log.DebugContext(ctx, "mitm.transparent.sniff_read_failed", "concern", "providers.mitm.wire", "err", err)
		}
		_ = conn.Close()
		return
	}
	client := &prefixConn{Conn: conn, prefix: []byte{firstByte[0]}}
	if firstByte[0] == transparentTLSHandshakeRecord {
		l.proxy.handleTransparentTLS(ctx, client)
		return
	}
	l.handoff(client)
}

func (l *sniffListener) handoff(conn net.Conn) {
	select {
	case l.conns <- conn:
	case <-l.done:
		_ = conn.Close()
	}
}

func (l *sniffListener) fail(err error) {
	select {
	case l.acceptErr <- err:
	default:
	}
	l.closeOnce.Do(func() {
		close(l.done)
		l.closeErr = l.Listener.Close()
	})
}

func (l *sniffListener) closed() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

func (p *Proxy) handleTransparentTLS(parentCtx context.Context, client net.Conn) {
	ctx := context.WithoutCancel(parentCtx)
	defer func() { _ = client.Close() }()
	started := clock.Now()
	ca, err := p.mitmCA()
	if err != nil {
		p.log.WarnContext(ctx, "mitm.transparent.ca_failed", "concern", "providers.mitm.wire", "err", err)
		return
	}
	var sniHost string
	tlsConn := tls.Server(client, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			sniHost = normalizeConnectHost(info.ServerName)
			if sniHost == "" {
				return nil, fmt.Errorf("transparent client hello has no SNI")
			}
			leaf, leafErr := ca.leafForHost(sniHost)
			if leafErr != nil {
				return nil, fmt.Errorf("mint transparent leaf for %q: %w", sniHost, leafErr)
			}
			nextProtos := mitmALPNProtocols(info.SupportedProtos)
			config := &tls.Config{
				Certificates: []tls.Certificate{*leaf},
				MinVersion:   tls.VersionTLS12,
				NextProtos:   nil,
			}
			if len(nextProtos) > 0 {
				config.NextProtos = nextProtos
			}
			return config, nil
		},
	})
	// Bound the handshake so a client that sends only the 0x16 record byte and
	// then stalls cannot pin this goroutine and its fd indefinitely. The sniff
	// read deadline was already cleared, and ctx has no cancellation, so this
	// deadline is the only bound on the ClientHello read.
	if err := client.SetDeadline(clock.Now().Add(transparentSniffTimeout)); err != nil {
		p.log.WarnContext(ctx, "mitm.transparent.set_handshake_deadline_failed", "concern", "providers.mitm.wire", "err", err)
		return
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		p.log.WarnContext(ctx, "mitm.transparent.client_tls_failed", "concern", "providers.mitm.wire", "host", sniHost, "err", err)
		return
	}
	if err := client.SetDeadline(time.Time{}); err != nil {
		p.log.WarnContext(ctx, "mitm.transparent.clear_handshake_deadline_failed", "concern", "providers.mitm.wire", "host", sniHost, "err", err)
		return
	}
	host := sniHost
	if host == "" {
		p.log.WarnContext(ctx, "mitm.transparent.missing_sni", "concern", "providers.mitm.wire")
		return
	}
	provider, claim, ok := providerForConnect(net.JoinHostPort(host, "443"))
	if !ok {
		p.log.DebugContext(ctx, "mitm.transparent.unclaimed_host", "concern", "providers.mitm.wire", "host", host)
		return
	}
	target := net.JoinHostPort(claim.Host, "443")
	p.serveInterceptedTLS(ctx, tlsConn, client, target, claim.Host, provider, started)
}
