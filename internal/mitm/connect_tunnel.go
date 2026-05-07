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
	p.log.InfoContext(ctx, "mitm.cursor.connect.intercept_open", "target", target, "host", host)
	p.serveCursorInterceptedHTTP(tlsConn, target, host)
	p.log.InfoContext(ctx, "mitm.cursor.connect.intercept_closed",
		"target", target,
		"host", host,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

func (p *Proxy) serveCursorInterceptedHTTP(client *tls.Conn, target string, host string) {
	reader := bufio.NewReader(client)
	writer := bufio.NewWriter(client)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.Debug("mitm.cursor.http.read_request_failed", "host", host, "err", err)
			}
			return
		}
		if err := p.handleCursorInterceptedRequest(writer, req, target, host); err != nil {
			p.log.Warn("mitm.cursor.http.request_failed", "host", host, "path", req.URL.Path, "err", err)
			return
		}
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
		return fmt.Errorf("read cursor request body: %w", err)
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.RequestURI = ""
	req.URL.Scheme = "https"
	req.URL.Host = target

	requestRawPath, responseRawPath, err := p.nextRawCapturePaths(cfg.CaptureDir, host, req.URL.Path)
	if err != nil {
		return fmt.Errorf("prepare raw capture paths: %w", err)
	}
	requestBytes, err := writeRawCaptureFile(requestRawPath, func(dst io.Writer) error {
		req.Body = io.NopCloser(bytes.NewReader(body))
		return req.Write(dst)
	})
	if err != nil {
		return fmt.Errorf("write raw cursor request: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))

	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
		TLSClientConfig:     p.cursorTLSClientConfig,
		DialContext:         p.dialContext,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	upstreamReq := req.Clone(req.Context())
	upstreamReq.Body = io.NopCloser(bytes.NewReader(body))
	upstreamReq.ContentLength = int64(len(body))
	upstreamReq.Host = host
	upstreamReq.URL.Scheme = "https"
	upstreamReq.URL.Host = target
	upstreamReq.RequestURI = ""
	upstreamReq.Header = req.Header.Clone()

	resp, err := transport.RoundTrip(upstreamReq)
	if err != nil {
		return fmt.Errorf("cursor upstream round trip: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	responseBytes, err := p.forwardAndCaptureCursorResponse(writer, resp, responseRawPath)
	if err != nil {
		return err
	}
	requestID, originalRequestID, sessionID, traceparent := extractCursorCaptureHeaders(req.Header)
	diag, hasDiag := cursorBidiAppendDiagnosticForRequest(&httpRequestCapture{
		Path:    req.URL.Path,
		Headers: req.Header,
		Body:    body,
	}, nil)
	if !hasDiag {
		diag = nil
	}
	if err := appendCursorCaptureMetadata(cfg.CaptureDir, cursorCaptureMetadata{
		Provider:            "cursor",
		Host:                host,
		Path:                req.URL.Path,
		Method:              req.Method,
		Status:              resp.StatusCode,
		RequestBytes:        requestBytes,
		ResponseBytes:       responseBytes,
		RequestRawPath:      requestRawPath,
		ResponseRawPath:     responseRawPath,
		RequestID:           requestID,
		OriginalRequestID:   originalRequestID,
		SessionID:           sessionID,
		Traceparent:         traceparent,
		RequestContentType:  req.Header.Get("Content-Type"),
		ResponseContentType: resp.Header.Get("Content-Type"),
		Diagnostic:          diag,
	}, capturePolicy); err != nil {
		p.log.Warn("mitm.cursor.capture.append_failed", "capture_dir", cfg.CaptureDir, "err", err)
	}
	p.log.Info("mitm.cursor.capture.completed",
		"host", host,
		"path", req.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", time.Since(started).Milliseconds(),
		"request_bytes", requestBytes,
		"response_bytes", responseBytes,
	)
	return nil
}

func (p *Proxy) forwardAndCaptureCursorResponse(client *bufio.Writer, resp *http.Response, responseRawPath string) (int64, error) {
	responseFile, err := os.OpenFile(responseRawPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
	if err != nil {
		p.log.Warn("mitm.cursor.response.open_capture_failed", "path", responseRawPath, "err", err)
		return 0, fmt.Errorf("open raw cursor response: %w", err)
	}
	defer func() { _ = responseFile.Close() }()

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
	if _, err := responseFile.Write(chunk); err != nil {
		slog.Warn("mitm.cursor.response.capture_write_failed", "label", label, "err", err)
		return 0, fmt.Errorf("capture cursor response %s: %w", label, err)
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
	if _, err := responseFile.WriteString(text); err != nil {
		slog.Warn("mitm.cursor.response.capture_write_failed", "label", label, "err", err)
		return 0, fmt.Errorf("capture cursor response %s: %w", label, err)
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.cursor.response.flush_failed", "label", label, "err", err)
		return 0, fmt.Errorf("flush cursor response %s: %w", label, err)
	}
	return int64(len(text)), nil
}
