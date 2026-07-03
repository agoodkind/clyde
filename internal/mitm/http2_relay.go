package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"

	"golang.org/x/net/http2"
	"goodkind.io/clyde/internal/livetrack"
)

func (p *Proxy) serveProviderInterceptedHTTP2(ctx context.Context, client *tls.Conn, target string, host string, provider Provider, parent *livetrack.Session[TunnelMeta]) {
	providerID := provider.ID().String()
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

	h2Server := p.h2Server
	if h2Server == nil {
		h2Server = &http2.Server{}
	}
	h2Server.ServeConn(client, &http2.ServeConnOpts{
		Context: ctx,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			p.handleProviderH2Stream(ctx, client, w, req, target, host, provider, parent, &activeRequests)
		}),
	})
}

// handleProviderH2Stream serves one HTTP/2 stream as the concurrent-goroutine
// analogue of one HTTP/1.1 loop iteration in serveProviderInterceptedHTTP1: it
// touches the parent tunnel, registers a per-stream livetrack child, and
// dispatches through the shared intercepted-request path with an h2 response
// sink. It uses the connection context ctx (the ServeConnOpts.Context) for
// livetrack and logging, matching the h1 loop; the per-stream request context
// still flows to the upstream round trip through the cloned req.
func (p *Proxy) handleProviderH2Stream(ctx context.Context, client *tls.Conn, w http.ResponseWriter, req *http.Request, target string, host string, provider Provider, parent *livetrack.Session[TunnelMeta], activeRequests *atomic.Int32) {
	providerID := provider.ID().String()
	parent.Touch()
	activeRequests.Add(1)
	defer activeRequests.Add(-1)

	closer := newTunnelCloser(&connCloser{conn: client}, nil)
	streamSession, registerErr := p.Tunnels.Register(ctx, "mitm."+providerID+".http2", TunnelMeta{
		ConnectHost:   host,
		UpstreamAddr:  target,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, closer, livetrack.WithParent(parent))
	if registerErr != nil {
		_ = req.Body.Close()
		if !errors.Is(registerErr, livetrack.ErrRegistryClosed) || parent == nil || p.Tunnels.State() != livetrack.StateDraining {
			p.log.WarnContext(ctx, "mitm.provider.http2.register_rejected", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", registerErr)
		} else {
			p.log.DebugContext(ctx, "mitm.provider.http2.request_rejected_reload_drain", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "err", registerErr)
		}
		http.Error(w, "service draining", http.StatusServiceUnavailable)
		return
	}
	releaseReason := "mitm." + providerID + ".http2.completed"
	defer func() {
		p.Tunnels.Release(ctx, streamSession, releaseReason)
	}()

	sink := &h2ProviderResponseSink{rw: w, session: streamSession, wroteHeader: false}
	if err := p.handleProviderInterceptedRequest(ctx, nil, nil, sink, req, target, host, provider, parent, streamSession); err != nil {
		releaseReason = "mitm." + providerID + ".http2.failed"
		p.log.WarnContext(ctx, "mitm.provider.http2.request_failed", "concern", "providers.mitm.wire", "provider", providerID, "host", host, "path", req.URL.Path, "err", err)
		// A failure before any header was written would otherwise leave the h2
		// stream with no explicit response; net/http2 implicitly closes it as a
		// 200 with an empty body, misleading the client into treating the
		// failure as success. A failure after headers were already sent cannot
		// be retroactively changed, so only respond here when nothing went out.
		if !sink.wroteHeader {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}
		return
	}
	parent.Touch()
	streamSession.Touch()
}

type h2ProviderResponseSink struct {
	rw          http.ResponseWriter
	session     *livetrack.Session[TunnelMeta]
	wroteHeader bool
}

func (s *h2ProviderResponseSink) writeProviderResponse(resp *http.Response, bodyCap int) (int64, []byte, error) {
	forwardResponseHeaders(s.rw.Header(), resp.Header)
	s.wroteHeader = true
	s.rw.WriteHeader(resp.StatusCode)

	captureBuffer := &limitedBuffer{limit: bodyCap, buf: bytes.Buffer{}}
	counter := &countingWriter{writer: s.rw, bytesWritten: 0}
	flusher, _ := s.rw.(http.Flusher)
	// streamWithFlush is same-package, so its error is returned unwrapped: the
	// caller (forwardProviderRequestToUpstream) owns logging, matching the h1
	// forwardAndCaptureProviderResponse contract.
	err := streamWithFlush(counter, captureBuffer, resp.Body, flusher, s.session)
	return counter.bytesWritten, captureBuffer.Bytes(), err
}

type countingWriter struct {
	writer       http.ResponseWriter
	bytesWritten int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.bytesWritten += int64(n)
	if err != nil {
		return n, fmt.Errorf("counting writer: %w", err)
	}
	return n, nil
}
