package mitm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
)

// tunnelCloser implements livetrack.Closer for one MITM tunnel. The
// closer terminates both ends of the splice (hijacked client conn
// and upstream conn) on force-close so wedged Cloudflare keepalive
// tunnels cannot pin reload past the registry's grace deadline.
//
// The struct holds [io.Closer] values rather than [net.Conn] so the
// same closer applies to plain TCP tunnels, intercepted TLS
// connections, and the [bufio.ReadWriter] pair used by the Cursor
// HTTP loop. Each Close call is idempotent: the closed flag flips
// on the first invocation so subsequent calls are no-ops.
type tunnelCloser struct {
	closed atomic.Bool
	client io.Closer
	upper  io.Closer
}

func newTunnelCloser(client io.Closer, upper io.Closer) *tunnelCloser {
	return &tunnelCloser{closed: atomic.Bool{}, client: client, upper: upper}
}

func (c *tunnelCloser) Close(reason string) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	var clientErr, upperErr error
	if c.client != nil {
		clientErr = c.client.Close()
	}
	if c.upper != nil {
		upperErr = c.upper.Close()
	}
	if clientErr != nil && upperErr != nil {
		err := fmt.Errorf("close tunnel client=%w upstream=%w", clientErr, upperErr)
		slog.Warn("mitm.tunnel.close_failed", "concern", "providers.mitm.wire", "component", "mitm", "reason", reason, "err", err)
		return err
	}
	if clientErr != nil {
		err := fmt.Errorf("close tunnel client: %w", clientErr)
		slog.Warn("mitm.tunnel.client_close_failed", "concern", "providers.mitm.wire", "component", "mitm", "reason", reason, "err", err)
		return err
	}
	if upperErr != nil {
		err := fmt.Errorf("close tunnel upstream: %w", upperErr)
		slog.Warn("mitm.tunnel.upstream_close_failed", "concern", "providers.mitm.wire", "component", "mitm", "reason", reason, "err", err)
		return err
	}
	return nil
}

// mitmHTTPCloser cancels the plain HTTP request context on force-close.
// Plain HTTP exchanges have no hijacked socket to terminate, so canceling
// the request context is the available shutdown mechanism.
type mitmHTTPCloser struct {
	cancel context.CancelFunc
}

func (c *mitmHTTPCloser) Close(string) error {
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

// connCloser wraps a [net.Conn] so the tunnelCloser can hold the
// concrete connection and the proxy can install a single Closer
// regardless of whether the connection was hijacked from net/http
// (yielding [net.TCPConn]) or terminated locally as a [tls.Conn].
type connCloser struct {
	conn net.Conn
}

func (c *connCloser) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	if err := c.conn.Close(); err != nil {
		wrapped := fmt.Errorf("close conn: %w", err)
		slog.Warn("mitm.conn.close_failed", "concern", "providers.mitm.wire", "component", "mitm", "err", wrapped)
		return wrapped
	}
	return nil
}
