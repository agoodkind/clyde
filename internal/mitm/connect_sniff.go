package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	"goodkind.io/clyde/internal/clock"
)

type unclaimedConnectStart struct {
	clientPrefix   []byte
	upstreamPrefix []byte
	upstream       net.Conn
	interceptTLS   bool
}

type connectByteRead struct {
	value byte
	ok    bool
	err   error
}

type connectDialRead struct {
	conn net.Conn
	err  error
}

func (p *Proxy) classifyUnclaimedConnectStart(ctx context.Context, clientConn net.Conn, cleanTarget string) (unclaimedConnectStart, bool) {
	clientCh := p.startConnectByteRead(ctx, clientConn)

	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	dialCh := p.startConnectDial(ctx, dialCtx, cleanTarget)

	var upstream net.Conn
	var upstreamCh <-chan connectByteRead
	var dialErr error
	for {
		select {
		case clientRead := <-clientCh:
			return p.unclaimedStartFromClientRead(ctx, clientStartInput{
				clientRead: clientRead,
				upstream:   upstream,
				upstreamCh: upstreamCh,
				dialCh:     dialCh,
				cancelDial: cancelDial,
				target:     cleanTarget,
				dialErr:    dialErr,
			})
		case dialRead := <-dialCh:
			dialCh = nil
			if dialRead.err != nil {
				dialErr = dialRead.err
				continue
			}
			upstream = dialRead.conn
			upstreamCh = p.startConnectByteRead(ctx, upstream)
		case upstreamRead := <-upstreamCh:
			return p.unclaimedStartFromUpstreamRead(ctx, clientConn, clientCh, upstream, upstreamRead, dialErr, cleanTarget)
		}
	}
}

type clientStartInput struct {
	clientRead connectByteRead
	upstream   net.Conn
	upstreamCh <-chan connectByteRead
	dialCh     <-chan connectDialRead
	cancelDial context.CancelFunc
	target     string
	dialErr    error
}

func (p *Proxy) unclaimedStartFromClientRead(ctx context.Context, input clientStartInput) (unclaimedConnectStart, bool) {
	if !input.clientRead.ok {
		p.logConnectSniffReadFailure(ctx, input.target, input.clientRead.err)
		p.logConnectDialFailure(ctx, input.target, input.dialErr)
		closeConnectUpstream(input.upstream)
		cleanupPendingConnectDial(ctx, p.log, input.dialCh, input.cancelDial)
		return emptyUnclaimedConnectStart(), false
	}
	clientPrefix := []byte{input.clientRead.value}
	if input.clientRead.value == transparentTLSHandshakeRecord {
		closeConnectUpstream(input.upstream)
		cleanupPendingConnectDial(ctx, p.log, input.dialCh, input.cancelDial)
		return unclaimedConnectStart{
			clientPrefix:   clientPrefix,
			upstreamPrefix: nil,
			upstream:       nil,
			interceptTLS:   true,
		}, true
	}
	upstreamRead := stopPendingConnectRead(input.upstream, input.upstreamCh)
	upstreamPrefix := connectBytePrefix(upstreamRead)
	upstream := input.upstream
	if upstream == nil {
		dialRead, ok := waitForConnectDial(input.dialCh, input.cancelDial)
		if !ok {
			p.logConnectDialFailure(ctx, input.target, input.dialErr)
			return emptyUnclaimedConnectStart(), false
		}
		if dialRead.err != nil {
			p.logConnectDialFailure(ctx, input.target, dialRead.err)
			return emptyUnclaimedConnectStart(), false
		}
		upstream = dialRead.conn
	}
	return unclaimedConnectStart{
		clientPrefix:   clientPrefix,
		upstreamPrefix: upstreamPrefix,
		upstream:       upstream,
		interceptTLS:   false,
	}, true
}

func (p *Proxy) unclaimedStartFromUpstreamRead(ctx context.Context, clientConn net.Conn, clientCh <-chan connectByteRead, upstream net.Conn, upstreamRead connectByteRead, dialErr error, target string) (unclaimedConnectStart, bool) {
	clientRead := stopPendingConnectRead(clientConn, clientCh)
	if clientRead.ok && clientRead.value == transparentTLSHandshakeRecord {
		closeConnectUpstream(upstream)
		return unclaimedConnectStart{
			clientPrefix:   []byte{clientRead.value},
			upstreamPrefix: nil,
			upstream:       nil,
			interceptTLS:   true,
		}, true
	}
	if upstream == nil {
		p.logConnectDialFailure(ctx, target, dialErr)
		return emptyUnclaimedConnectStart(), false
	}
	clientPrefix := connectBytePrefix(clientRead)
	upstreamPrefix := connectBytePrefix(upstreamRead)
	if len(clientPrefix) == 0 && len(upstreamPrefix) == 0 {
		p.logConnectSniffReadFailure(ctx, target, upstreamRead.err)
		closeConnectUpstream(upstream)
		return emptyUnclaimedConnectStart(), false
	}
	return unclaimedConnectStart{
		clientPrefix:   clientPrefix,
		upstreamPrefix: upstreamPrefix,
		upstream:       upstream,
		interceptTLS:   false,
	}, true
}

func (p *Proxy) startConnectByteRead(ctx context.Context, conn net.Conn) <-chan connectByteRead {
	readCh := make(chan connectByteRead, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("connect byte read panic: %v", recovered)
				p.log.ErrorContext(ctx, "mitm.connect.sniff_read_panicked", "concern", "providers.mitm.errors", "err", err)
				readCh <- connectByteRead{value: 0, ok: false, err: err}
			}
		}()
		readCh <- readConnectByte(conn, transparentSniffTimeout)
	}()
	return readCh
}

func (p *Proxy) startConnectDial(ctx context.Context, dialCtx context.Context, target string) <-chan connectDialRead {
	dialCh := make(chan connectDialRead, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("connect dial panic: %v", recovered)
				p.log.ErrorContext(ctx, "mitm.connect.upstream_dial_panicked", "concern", "providers.mitm.errors", "target", target, "err", err)
				dialCh <- connectDialRead{conn: nil, err: err}
			}
		}()
		upstream, err := new(net.Dialer).DialContext(dialCtx, "tcp", target)
		dialCh <- connectDialRead{conn: upstream, err: err}
	}()
	return dialCh
}

func readConnectByte(conn net.Conn, timeout time.Duration) connectByteRead {
	if err := conn.SetReadDeadline(clock.Now().Add(timeout)); err != nil {
		return connectByteRead{value: 0, ok: false, err: err}
	}
	var firstByte [1]byte
	n, readErr := conn.Read(firstByte[:])
	// Clear the deadline regardless of the read result. If clearing fails the
	// connection is left with an active deadline that would spuriously time out
	// later reads (the TLS handshake or the splice), so treat that as a read
	// failure even when a byte arrived rather than hand back a broken conn.
	if clearErr := conn.SetReadDeadline(time.Time{}); clearErr != nil {
		return connectByteRead{value: 0, ok: false, err: clearErr}
	}
	if n == 0 {
		return connectByteRead{value: 0, ok: false, err: readErr}
	}
	return connectByteRead{value: firstByte[0], ok: true, err: nil}
}

func (p *Proxy) logConnectSniffReadFailure(ctx context.Context, target string, err error) {
	if err == nil {
		return
	}
	p.log.DebugContext(ctx, "mitm.connect.sniff_read_failed", "concern", "providers.mitm.errors", "target", target, "err", err)
}

func (p *Proxy) logConnectDialFailure(ctx context.Context, target string, err error) {
	if err == nil {
		return
	}
	p.log.WarnContext(ctx, "mitm.connect.upstream_dial_failed", "concern", "providers.mitm.errors", "target", target, "err", err)
}

func connectBytePrefix(read connectByteRead) []byte {
	if !read.ok {
		return nil
	}
	return []byte{read.value}
}

func closeConnectUpstream(upstream net.Conn) {
	if upstream != nil {
		_ = upstream.Close()
	}
}

func cleanupPendingConnectDial(ctx context.Context, log *slog.Logger, dialCh <-chan connectDialRead, cancelDial context.CancelFunc) {
	if dialCh == nil {
		return
	}
	cancelDial()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("connect dial cleanup panic: %v", recovered)
				log.ErrorContext(ctx, "mitm.connect.upstream_dial_cleanup_panicked", "concern", "providers.mitm.errors", "err", err)
			}
		}()
		dialRead := <-dialCh
		closeConnectUpstream(dialRead.conn)
	}()
}

func waitForConnectDial(dialCh <-chan connectDialRead, cancelDial context.CancelFunc) (connectDialRead, bool) {
	if dialCh == nil {
		return connectDialRead{conn: nil, err: nil}, false
	}
	defer cancelDial()
	return <-dialCh, true
}

func stopPendingConnectRead(conn net.Conn, readCh <-chan connectByteRead) connectByteRead {
	if readCh == nil {
		return connectByteRead{value: 0, ok: false, err: nil}
	}
	// Expire the deadline to interrupt the pending read, collect its result,
	// then clear the deadline so the connection is usable for the splice or the
	// terminated TLS handshake that follows. Leaving the past deadline set would
	// make every later read on this connection time out immediately.
	_ = conn.SetReadDeadline(clock.Now())
	read := <-readCh
	_ = conn.SetReadDeadline(time.Time{})
	return read
}

func emptyUnclaimedConnectStart() unclaimedConnectStart {
	return unclaimedConnectStart{
		clientPrefix:   nil,
		upstreamPrefix: nil,
		upstream:       nil,
		interceptTLS:   false,
	}
}
