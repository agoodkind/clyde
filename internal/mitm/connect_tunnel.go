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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// handleConnect implements RFC 7230 section 4.3.6 HTTP CONNECT
// tunneling. Clients like codex-cli reach wss://chatgpt.com through
// the proxy by issuing CONNECT chatgpt.com:443 and then speaking
// TLS+websocket on the resulting stream. Without this handler the
// default mux returns 404 and the client cannot establish the
// upstream connection.
//
// Tunnel mode forwards opaque bytes in both directions. We do not
// terminate TLS, so the tunneled bytes are end-to-end encrypted; the
// payload is not captured in the JSONL transcript. The capture path
// only sees the CONNECT request line and the duration of the
// tunnel. This is intentional: terminating TLS would require a
// per-host certificate and cert pinning would break the upstream's
// trust anyway.
//
// Drift detection for upstreams that exclusively use CONNECT (e.g.
// codex-cli's wss://chatgpt.com path) needs an external mitmproxy
// session because we deliberately do not MITM-decrypt at this
// layer.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	started := currentTime()
	target := strings.TrimSpace(r.RequestURI)
	if target == "" {
		target = strings.TrimSpace(r.Host)
	}
	if target == "" {
		http.Error(w, "missing CONNECT target", http.StatusBadRequest)
		return
	}
	host, _, err := net.SplitHostPort(target)
	if err != nil || host == "" {
		http.Error(w, "invalid CONNECT target: "+target, http.StatusBadRequest)
		return
	}
	if cursorHost, ok := shouldInterceptCursorConnect(target); ok {
		p.handleCursorTLSConnect(r.Context(), w, target, cursorHost, started)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	upstream, err := net.DialTimeout("tcp", target, 30*time.Second)
	if err != nil {
		p.log.Warn("mitm.connect.upstream_dial_failed",
			"target", target,
			"err", err,
		)
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		p.log.Warn("mitm.connect.hijack_failed", "target", target, "err", err)
		_ = upstream.Close()
		return
	}
	defer func() { _ = clientConn.Close() }()

	// Tell the client the tunnel is established. The client will
	// follow with TLS handshake + websocket frames.
	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.log.Warn("mitm.connect.write_established_failed", "err", err)
		return
	}
	if err := bufrw.Flush(); err != nil {
		p.log.Warn("mitm.connect.flush_failed", "err", err)
		return
	}

	closer := newTunnelCloser(&connCloser{conn: clientConn}, &connCloser{conn: upstream})
	sess, err := p.Tunnels.Register(r.Context(), "mitm.connect", TunnelMeta{
		ConnectHost:   host,
		UpstreamAddr:  target,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, closer)
	if err != nil {
		p.log.WarnContext(r.Context(), "mitm.connect.register_rejected", "target", target, "err", err)
		return
	}
	defer p.Tunnels.Release(r.Context(), sess, "mitm.connect.tunnel_closed")

	p.log.Info("mitm.connect.tunnel_open",
		"target", target,
		"host", host,
	)
	bytesUp, bytesDown := spliceConnections(clientConn, upstream)
	p.log.Info("mitm.connect.tunnel_closed",
		"target", target,
		"host", host,
		"duration_ms", time.Since(started).Milliseconds(),
		"bytes_up", bytesUp,
		"bytes_down", bytesDown,
	)
}

// spliceConnections forwards bytes between two connections in both
// directions until one side closes. Returns the count of bytes that
// flowed in each direction.
func spliceConnections(client, upstream net.Conn) (bytesUp, bytesDown int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	var upN, downN int64
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("mitm.connect.copy_up_panic",
					"component", "mitm",
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		defer wg.Done()
		n, _ := io.Copy(upstream, client)
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
				slog.Error("mitm.connect.copy_down_panic",
					"component", "mitm",
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		defer wg.Done()
		n, _ := io.Copy(client, upstream)
		downN = n
		if cw, ok := client.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	wg.Wait()
	return upN, downN
}

func (p *Proxy) handleCursorTLSConnect(ctx context.Context, w http.ResponseWriter, target string, host string, started time.Time) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		p.log.WarnContext(ctx, "mitm.cursor.connect.hijack_failed", "target", target, "err", err)
		return
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.log.WarnContext(ctx, "mitm.cursor.connect.write_established_failed", "target", target, "err", err)
		return
	}
	if err := bufrw.Flush(); err != nil {
		p.log.WarnContext(ctx, "mitm.cursor.connect.flush_failed", "target", target, "err", err)
		return
	}

	ca, err := p.cursorCA()
	if err != nil {
		p.log.WarnContext(ctx, "mitm.cursor.connect.ca_failed", "target", target, "err", err)
		return
	}
	leaf, err := ca.leafForHost(host)
	if err != nil {
		p.log.WarnContext(ctx, "mitm.cursor.connect.leaf_failed", "target", target, "host", host, "err", err)
		return
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		p.log.WarnContext(ctx, "mitm.cursor.connect.client_tls_failed", "target", target, "host", host, "err", err)
		return
	}
	closer := newTunnelCloser(&connCloser{conn: tlsConn}, &connCloser{conn: clientConn})
	sess, err := p.Tunnels.Register(ctx, "mitm.cursor.tls", TunnelMeta{
		ConnectHost:   host,
		UpstreamAddr:  target,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, closer)
	if err != nil {
		p.log.WarnContext(ctx, "mitm.cursor.connect.register_rejected", "target", target, "err", err)
		return
	}
	defer p.Tunnels.Release(ctx, sess, "mitm.cursor.tls.closed")
	p.log.InfoContext(ctx, "mitm.cursor.connect.intercept_open", "target", target, "host", host)
	p.serveCursorInterceptedHTTP(ctx, tlsConn, target, host, sess)
	p.log.InfoContext(ctx, "mitm.cursor.connect.intercept_closed",
		"target", target,
		"host", host,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

func (p *Proxy) serveCursorInterceptedHTTP(ctx context.Context, client *tls.Conn, target string, host string, parent *livetrack.Session[TunnelMeta]) {
	reader := bufio.NewReader(client)
	writer := bufio.NewWriter(client)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.DebugContext(ctx, "mitm.cursor.http.read_request_failed", "host", host, "err", err)
			}
			return
		}
		// Register per-request session so the daemon's reload drain
		// sees in-flight cursor exchanges, not just the parent TLS
		// session. The closer terminates the underlying TLS conn so
		// force-close interrupts a hung intercepted request. The
		// parent linkage records the TLS session id so operators can
		// correlate request bursts back to their CONNECT tunnel.
		closer := newTunnelCloser(&connCloser{conn: client}, nil)
		reqSess, registerErr := p.Tunnels.Register(ctx, "mitm.cursor.http", TunnelMeta{
			ConnectHost:   host,
			UpstreamAddr:  target,
			CaptureFile:   "",
			KeepaliveSeen: false,
		}, closer, livetrack.WithParent(parent))
		if registerErr != nil {
			p.log.WarnContext(ctx, "mitm.cursor.http.register_rejected", "host", host, "err", registerErr)
			return
		}
		if err := p.handleCursorInterceptedRequest(writer, req, target, host); err != nil {
			p.log.WarnContext(ctx, "mitm.cursor.http.request_failed", "host", host, "path", req.URL.Path, "err", err)
			p.Tunnels.Release(ctx, reqSess, "mitm.cursor.http.failed")
			return
		}
		p.Tunnels.Release(ctx, reqSess, "mitm.cursor.http.completed")
		if req.Close {
			return
		}
	}
}

func (p *Proxy) handleCursorInterceptedRequest(writer *bufio.Writer, req *http.Request, target string, host string) error {
	started := currentTime()
	cfg := p.config()
	capturePolicy := captureFilePolicyFromConfig(cfg)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		p.log.WarnContext(req.Context(), "mitm.cursor.request.read_body_failed", "host", host, "err", err)
		return fmt.Errorf("read cursor request body: %w", err)
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.RequestURI = ""
	req.URL.Scheme = "https"
	req.URL.Host = target

	concern := resolveCaptureConcern(cfg.CaptureRules, captureConcernInput{
		Provider:            "cursor",
		Host:                host,
		Method:              req.Method,
		Path:                req.URL.Path,
		RequestContentType:  req.Header.Get("Content-Type"),
		ResponseContentType: "",
	})
	var requestRawPath string
	var responseRawPath string
	requestBytes := int64(len(body))
	if cfg.RawCaptureEnabled {
		requestRawPath, responseRawPath, err = p.nextRawCapturePaths(cfg.CaptureDir, concern, host, req.URL.Path)
		if err != nil {
			p.log.WarnContext(req.Context(), "mitm.cursor.request.prepare_raw_paths_failed", "host", host, "err", err)
			return fmt.Errorf("prepare raw capture paths: %w", err)
		}
		requestBytes, err = writeRawCaptureFile(requestRawPath, func(dst io.Writer) error {
			req.Body = io.NopCloser(bytes.NewReader(body))
			return req.Write(dst)
		})
		if err != nil {
			p.log.WarnContext(req.Context(), "mitm.cursor.request.write_raw_failed", "host", host, "err", err)
			return fmt.Errorf("write raw cursor request: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	if rule, ok := matchHookRule(cfg.Hooks, host, req.Method, req.URL.Path); ok {
		return p.runHookedCursorRequest(req.Context(), hookedCursorParams{
			writer:          writer,
			req:             req,
			body:            body,
			target:          target,
			host:            host,
			rule:            rule,
			started:         started,
			concern:         concern,
			requestBytes:    requestBytes,
			requestRawPath:  requestRawPath,
			responseRawPath: responseRawPath,
			cfg:             cfg,
			capturePolicy:   capturePolicy,
		})
	}

	return p.forwardCursorRequestToUpstream(cursorForwardParams{
		writer:          writer,
		req:             req,
		body:            body,
		target:          target,
		host:            host,
		started:         started,
		concern:         concern,
		requestBytes:    requestBytes,
		requestRawPath:  requestRawPath,
		responseRawPath: responseRawPath,
		cfg:             cfg,
		capturePolicy:   capturePolicy,
	})
}

// cursorForwardParams bundles the per-request state computed inside
// handleCursorInterceptedRequest so the forwarding helper can run the
// upstream round-trip and capture-metadata write without a wide
// parameter list.
type cursorForwardParams struct {
	writer          *bufio.Writer
	req             *http.Request
	body            []byte
	target          string
	host            string
	started         time.Time
	concern         string
	requestBytes    int64
	requestRawPath  string
	responseRawPath string
	cfg             config.MITMConfig
	capturePolicy   CaptureFilePolicy
}

// forwardCursorRequestToUpstream runs the standard (non-hook)
// pass-through path: round-trip to upstream, stream the response to
// the client, and append capture metadata. Split out of
// handleCursorInterceptedRequest to keep both functions under the
// funlen ceiling.
func (p *Proxy) forwardCursorRequestToUpstream(params cursorForwardParams) error {
	resp, err := p.cursorUpstreamRoundTrip(params.req, params.body, params.target, params.host)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	responseBytes, err := p.forwardAndCaptureCursorResponse(params.writer, resp, params.responseRawPath)
	if err != nil {
		return err
	}
	requestID, originalRequestID, sessionID, traceparent := extractCursorCaptureHeaders(params.req.Header)
	diag, hasDiag := cursorBidiAppendDiagnosticForRequest(&httpRequestCapture{
		Path:    params.req.URL.Path,
		Headers: params.req.Header,
		Body:    params.body,
	}, nil)
	if !hasDiag {
		diag = nil
	}
	if appendErr := p.appendCursorCaptureMetadata(params.cfg.CaptureDir, cursorCaptureMetadata{
		Provider:            "cursor",
		Concern:             params.concern,
		Host:                params.host,
		Path:                params.req.URL.Path,
		Method:              params.req.Method,
		Status:              resp.StatusCode,
		RequestBytes:        params.requestBytes,
		ResponseBytes:       responseBytes,
		RequestRawPath:      params.requestRawPath,
		ResponseRawPath:     params.responseRawPath,
		RequestID:           requestID,
		OriginalRequestID:   originalRequestID,
		SessionID:           sessionID,
		Traceparent:         traceparent,
		RequestContentType:  params.req.Header.Get("Content-Type"),
		ResponseContentType: resp.Header.Get("Content-Type"),
		Diagnostic:          diag,
		Hook:                "",
	}, params.capturePolicy); appendErr != nil {
		if errors.Is(appendErr, ErrCaptureSinkClosed) {
			p.log.Debug("mitm.cursor.capture.append_skipped_closed",
				"component", "mitm",
				"concern", "providers.mitm.wire",
				"capture_dir", params.cfg.CaptureDir,
			)
		} else {
			p.log.Warn("mitm.cursor.capture.append_failed",
				"component", "mitm",
				"concern", "providers.mitm.wire",
				"capture_dir", params.cfg.CaptureDir,
				"err", appendErr,
			)
		}
	}
	p.log.Info("mitm.cursor.capture.completed",
		"host", params.host,
		"concern", params.concern,
		"path", params.req.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", time.Since(params.started).Milliseconds(),
		"request_bytes", params.requestBytes,
		"response_bytes", responseBytes,
	)
	return nil
}

// cursorUpstreamRoundTrip dials the upstream over the proxy's TLS
// client config and returns the response. Extracted from
// handleCursorInterceptedRequest to keep that function below the
// funlen threshold; the transport is constructed per-request and its
// idle connections are closed on return so this helper owns the
// transport's lifetime end-to-end.
func (p *Proxy) cursorUpstreamRoundTrip(req *http.Request, body []byte, target string, host string) (*http.Response, error) {
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
		TLSClientConfig:     p.cursorTLSClientConfig,
		DialContext:         p.dialContext,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(cursorUpstreamRequest(req, body, target, host))
	if err != nil {
		p.log.Warn("mitm.cursor.upstream_round_trip_failed",
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

func cursorUpstreamRequest(req *http.Request, body []byte, target string, host string) *http.Request {
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

func (p *Proxy) forwardAndCaptureCursorResponse(client *bufio.Writer, resp *http.Response, responseRawPath string) (int64, error) {
	var responseFile *os.File
	if responseRawPath != "" {
		var err error
		responseFile, err = os.OpenFile(responseRawPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
		if err != nil {
			p.log.Warn("mitm.cursor.response.open_capture_failed", "path", responseRawPath, "err", err)
			return 0, fmt.Errorf("open raw cursor response: %w", err)
		}
		defer func() { _ = responseFile.Close() }()
	}

	chunked := resp.ContentLength < 0
	header := cursorResponseHeader(resp, chunked)
	responseBytes, err := writeCursorResponseBytes(client, responseFile, header, "header")
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, err := writeCursorResponseBodyChunk(client, responseFile, buf[:n], chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			written, err := writeCursorResponseEOF(client, responseFile, chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, err
			}
			return responseBytes, nil
		}
		if readErr != nil {
			p.log.Warn("mitm.cursor.response.read_body_failed", "err", readErr)
			return responseBytes, fmt.Errorf("read cursor response body: %w", readErr)
		}
	}
}

func cursorResponseHeader(resp *http.Response, chunked bool) []byte {
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

func writeCursorResponseBodyChunk(client *bufio.Writer, responseFile *os.File, chunk []byte, chunked bool) (int64, error) {
	var written int64
	if chunked {
		chunkHeader := fmt.Appendf(nil, "%x\r\n", len(chunk))
		count, err := writeCursorResponseBytes(client, responseFile, chunkHeader, "chunk header")
		written += count
		if err != nil {
			return written, err
		}
	}
	count, err := writeCursorResponseBytes(client, responseFile, chunk, "body")
	written += count
	if err != nil {
		return written, err
	}
	if chunked {
		count, err = writeCursorResponseString(client, responseFile, "\r\n", "chunk terminator")
		written += count
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func writeCursorResponseEOF(client *bufio.Writer, responseFile *os.File, chunked bool) (int64, error) {
	if !chunked {
		if err := client.Flush(); err != nil {
			slog.Warn("mitm.cursor.response.flush_body_failed", "err", err)
			return 0, fmt.Errorf("flush cursor response body: %w", err)
		}
		return 0, nil
	}
	return writeCursorResponseString(client, responseFile, "0\r\n\r\n", "chunk EOF")
}

func writeCursorResponseBytes(client *bufio.Writer, responseFile *os.File, chunk []byte, label string) (int64, error) {
	if _, err := client.Write(chunk); err != nil {
		slog.Warn("mitm.cursor.response.write_client_failed", "label", label, "err", err)
		return 0, fmt.Errorf("write cursor response %s: %w", label, err)
	}
	if responseFile != nil {
		if _, err := responseFile.Write(chunk); err != nil {
			slog.Warn("mitm.cursor.response.capture_write_failed", "label", label, "err", err)
			return 0, fmt.Errorf("capture cursor response %s: %w", label, err)
		}
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.cursor.response.flush_failed", "label", label, "err", err)
		return 0, fmt.Errorf("flush cursor response %s: %w", label, err)
	}
	return int64(len(chunk)), nil
}

func writeCursorResponseString(client *bufio.Writer, responseFile *os.File, text string, label string) (int64, error) {
	if _, err := client.WriteString(text); err != nil {
		slog.Warn("mitm.cursor.response.write_client_failed", "label", label, "err", err)
		return 0, fmt.Errorf("write cursor response %s: %w", label, err)
	}
	if responseFile != nil {
		if _, err := responseFile.WriteString(text); err != nil {
			slog.Warn("mitm.cursor.response.capture_write_failed", "label", label, "err", err)
			return 0, fmt.Errorf("capture cursor response %s: %w", label, err)
		}
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.cursor.response.flush_failed", "label", label, "err", err)
		return 0, fmt.Errorf("flush cursor response %s: %w", label, err)
	}
	return int64(len(text)), nil
}
