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
	"sync/atomic"
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
// Provider-owned CONNECT hosts terminate TLS inside Clyde with a
// generated leaf certificate, then route the decoded HTTP request
// through the provider capture path. Unclaimed CONNECT hosts keep
// opaque tunnel mode and forward bytes without payload capture.
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
	if provider, claim, ok := providerForConnect(target); ok {
		p.handleProviderTLSConnect(r.Context(), w, target, claim.Host, provider, started)
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

func (p *Proxy) handleProviderTLSConnect(ctx context.Context, w http.ResponseWriter, target string, host string, provider Provider, started time.Time) {
	providerID := string(provider.ID())
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.hijack_failed", "provider", providerID, "target", target, "err", err)
		return
	}
	defer func() { _ = clientConn.Close() }()

	if _, err := bufrw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.write_established_failed", "provider", providerID, "target", target, "err", err)
		return
	}
	if err := bufrw.Flush(); err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.flush_failed", "provider", providerID, "target", target, "err", err)
		return
	}

	ca, err := p.mitmCA()
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.ca_failed", "provider", providerID, "target", target, "err", err)
		return
	}
	leaf, err := ca.leafForHost(host)
	if err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.leaf_failed", "provider", providerID, "target", target, "host", host, "err", err)
		return
	}
	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		p.log.WarnContext(ctx, "mitm.provider.connect.client_tls_failed", "provider", providerID, "target", target, "host", host, "err", err)
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
		p.log.WarnContext(ctx, "mitm.provider.connect.register_rejected", "provider", providerID, "target", target, "err", err)
		return
	}
	defer p.Tunnels.Release(ctx, sess, "mitm."+providerID+".tls.closed")
	p.log.InfoContext(ctx, "mitm.provider.connect.intercept_open", "provider", providerID, "target", target, "host", host)
	p.serveProviderInterceptedHTTP(ctx, tlsConn, target, host, provider, sess)
	p.log.InfoContext(ctx, "mitm.provider.connect.intercept_closed",
		"provider", providerID,
		"target", target,
		"host", host,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

func (p *Proxy) serveProviderInterceptedHTTP(ctx context.Context, client *tls.Conn, target string, host string, provider Provider, parent *livetrack.Session[TunnelMeta]) {
	providerID := string(provider.ID())
	reader := bufio.NewReader(client)
	writer := bufio.NewWriter(client)
	stopWatcher := make(chan struct{})
	var activeRequests atomic.Int32
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.log.WarnContext(ctx, "mitm.provider.tls.drain_watcher_panicked",
					"provider", providerID,
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
				p.log.DebugContext(ctx, "mitm.provider.http.read_request_failed", "provider", providerID, "host", host, "err", err)
			}
			return
		}
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
				p.log.WarnContext(ctx, "mitm.provider.http.register_rejected", "provider", providerID, "host", host, "err", registerErr)
				return
			}
			activeRequests.Add(-1)
			p.log.DebugContext(ctx, "mitm.provider.http.request_rejected_reload_drain", "provider", providerID, "host", host, "err", registerErr)
			return
		}
		if err := p.handleProviderInterceptedRequest(ctx, client, reader, writer, req, target, host, provider); err != nil {
			activeRequests.Add(-1)
			p.log.WarnContext(ctx, "mitm.provider.http.request_failed", "provider", providerID, "host", host, "path", req.URL.Path, "err", err)
			if reqSess != nil {
				p.Tunnels.Release(ctx, reqSess, "mitm."+providerID+".http.failed")
			}
			return
		}
		activeRequests.Add(-1)
		if reqSess != nil {
			p.Tunnels.Release(ctx, reqSess, "mitm."+providerID+".http.completed")
		}
		if req.Close {
			return
		}
		if parent != nil && p.Tunnels.State() == livetrack.StateDraining {
			p.log.DebugContext(ctx, "mitm.provider.http.keepalive_closed_reload_drain", "provider", providerID, "host", host, "path", req.URL.Path)
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
			p.log.DebugContext(ctx, "mitm.provider.tls.closed_reload_drain", "provider", providerID, "host", host)
			_ = client.Close()
			return
		}
	}
}

func buildProviderFailureInput(params providerForwardParams, requestIndex captureBodyIndex, responseIndex captureBodyIndex, statusCode int) httpCaptureRecordInput {
	return buildHTTPFailureCaptureInput(
		params.cfg,
		params.capturePolicy,
		string(params.provider.ID()),
		"https://"+params.host+params.req.URL.RequestURI(),
		params.body,
		requestIndex,
		responseIndex,
		time.Since(params.started),
		statusCode,
	)
}

func (p *Proxy) recordProviderFailure(req *http.Request, responseHeader http.Header, input httpCaptureRecordInput, failure httpFailureRecord) error {
	p.recordHTTPFailure(req, responseHeader, input, failure)
	return fmt.Errorf(failure.errorCode+": %s", failure.errorMessage)
}

func (p *Proxy) prepareProviderRequestCapture(req *http.Request, params providerForwardParams) (providerForwardParams, error) {
	if !params.cfg.RawCaptureEnabled {
		return params, nil
	}
	requestRawPath, responseRawPath, err := p.nextRawCapturePaths(params.cfg.CaptureDir, params.concern, params.host, params.req.URL.Path)
	if err != nil {
		return params, p.recordProviderFailure(req, http.Header{}, buildProviderFailureInput(params, emptyCaptureBodyIndex(), emptyCaptureBodyIndex(), http.StatusInternalServerError), httpFailureRecord{
			includePayload:      true,
			includeUpstreamSend: false,
			errorCode:           "prepare_raw_paths_failed",
			errorMessage:        err.Error(),
		})
	}
	requestBytes, err := writeRawCaptureFile(requestRawPath, func(dst io.Writer) error {
		req.Body = io.NopCloser(bytes.NewReader(params.body))
		return req.Write(dst)
	})
	if err != nil {
		return params, p.recordProviderFailure(req, http.Header{}, buildProviderFailureInput(params, emptyCaptureBodyIndex(), emptyCaptureBodyIndex(), http.StatusInternalServerError), httpFailureRecord{
			includePayload:      true,
			includeUpstreamSend: false,
			errorCode:           "write_raw_request_failed",
			errorMessage:        err.Error(),
		})
	}
	params.requestBytes = requestBytes
	params.requestRawPath = requestRawPath
	params.responseRawPath = responseRawPath
	req.Body = io.NopCloser(bytes.NewReader(params.body))
	return params, nil
}

func (p *Proxy) handleProviderInterceptedRequest(ctx context.Context, client *tls.Conn, reader *bufio.Reader, writer *bufio.Writer, req *http.Request, target string, host string, provider Provider) error {
	started := currentTime()
	cfg := p.config()
	capturePolicy := captureFilePolicyFromConfig(cfg)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return p.recordProviderFailure(req, http.Header{}, buildHTTPFailureCaptureInput(
			cfg,
			capturePolicy,
			string(provider.ID()),
			"https://"+host+req.URL.RequestURI(),
			nil,
			emptyCaptureBodyIndex(),
			emptyCaptureBodyIndex(),
			time.Since(started),
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
		if client == nil || reader == nil {
			return fmt.Errorf("provider websocket requires intercepted TLS connection")
		}
		return p.handleProviderInterceptedWebsocket(ctx, client, reader, writer, req, target, host, provider)
	}

	concern := resolveCaptureConcern(cfg.CaptureRules, captureConcernInput{
		Provider:            string(provider.ID()),
		Host:                host,
		Method:              req.Method,
		Path:                req.URL.Path,
		RequestContentType:  req.Header.Get("Content-Type"),
		ResponseContentType: "",
	})
	requestBytes := int64(len(body))
	params := providerForwardParams{
		writer:          writer,
		req:             req,
		body:            body,
		target:          target,
		host:            host,
		provider:        provider,
		started:         started,
		concern:         concern,
		requestBytes:    requestBytes,
		requestRawPath:  "",
		responseRawPath: "",
		cfg:             cfg,
		capturePolicy:   capturePolicy,
	}
	params, err = p.prepareProviderRequestCapture(req, params)
	if err != nil {
		return err
	}

	if rule, ok := matchHookRule(cfg.Hooks, host, req.Method, req.URL.Path); ok {
		return p.runHookedProviderRequest(ctx, hookedProviderParams{
			writer:          writer,
			req:             req,
			body:            body,
			target:          target,
			host:            host,
			provider:        provider,
			rule:            rule,
			started:         started,
			concern:         concern,
			requestBytes:    params.requestBytes,
			requestRawPath:  params.requestRawPath,
			responseRawPath: params.responseRawPath,
			cfg:             cfg,
			capturePolicy:   capturePolicy,
		})
	}

	return p.forwardProviderRequestToUpstream(params)
}

// providerForwardParams bundles the per-request state computed inside
// handleProviderInterceptedRequest so the forwarding helper can run the
// upstream round-trip and capture-metadata write without a wide
// parameter list.
type providerForwardParams struct {
	writer          *bufio.Writer
	req             *http.Request
	body            []byte
	target          string
	host            string
	provider        Provider
	started         time.Time
	concern         string
	requestBytes    int64
	requestRawPath  string
	responseRawPath string
	cfg             config.MITMConfig
	capturePolicy   CaptureFilePolicy
}

// forwardProviderRequestToUpstream runs the standard (non-hook)
// pass-through path: round-trip to upstream, stream the response to
// the client, and append capture metadata. Split out of
// handleProviderInterceptedRequest to keep both functions under the
// funlen ceiling.
func (p *Proxy) forwardProviderRequestToUpstream(params providerForwardParams) error {
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

	responseBytes, err := p.forwardAndCaptureProviderResponse(params.writer, resp, params.responseRawPath)
	if err != nil {
		return err
	}
	decodedRequestBody, decoded := decodeForCapture(params.body, params.req.Header.Get("Content-Encoding"))
	if !decoded {
		decodedRequestBody = params.body
	}
	if appendErr := p.appendProviderCaptureExtension(params.cfg.CaptureDir, params.provider.BuildCaptureExtension(CaptureExchange{
		CapturedAt:          currentTime().UTC(),
		RequestHeader:       params.req.Header,
		RequestBody:         params.body,
		DecodedRequestBody:  decodedRequestBody,
		ResponseHeader:      resp.Header,
		ResponseStatus:      resp.StatusCode,
		RequestBytes:        params.requestBytes,
		ResponseBytes:       responseBytes,
		Method:              params.req.Method,
		Path:                params.req.URL.Path,
		Host:                params.host,
		Concern:             params.concern,
		RequestRawPath:      params.requestRawPath,
		ResponseRawPath:     params.responseRawPath,
		RequestContentType:  params.req.Header.Get("Content-Type"),
		ResponseContentType: resp.Header.Get("Content-Type"),
		HookName:            "",
	}), params.capturePolicy); appendErr != nil {
		if errors.Is(appendErr, ErrCaptureSinkClosed) {
			p.log.Debug("mitm.provider.capture.append_skipped_closed",
				"component", "mitm",
				"concern", "providers.mitm.wire",
				"capture_dir", params.cfg.CaptureDir,
			)
		} else {
			p.log.Warn("mitm.provider.capture.append_failed",
				"component", "mitm",
				"concern", "providers.mitm.wire",
				"capture_dir", params.cfg.CaptureDir,
				"err", appendErr,
			)
		}
	}
	p.log.Info("mitm.provider.capture.completed",
		"provider", string(params.provider.ID()),
		"host", params.host,
		"concern", params.concern,
		"path", params.req.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", time.Since(params.started).Milliseconds(),
		"request_bytes", params.requestBytes,
		"response_bytes", responseBytes,
	)
	p.recordHTTPCapture(params.req, resp.Header, providerHTTPCaptureRecordInput(params, resp.StatusCode, responseBytes))
	return nil
}

// providerHTTPCaptureRecordInput packages the provider request and the
// upstream response shape into the shared httpCaptureRecordInput used
// by recordHTTPCapture. Provider TLS-intercept traffic does not summarize bodies in
// the same way the plain HTTP path does; the captureBodyIndex entries
// carry the raw file paths (and byte counts) so the leg events can
// surface raw_request_path / raw_response_path via the MITM facet without
// reintroducing summary captures.
func providerHTTPCaptureRecordInput(params providerForwardParams, statusCode int, responseBytes int64) httpCaptureRecordInput {
	requestRef := rawBodyReference{
		Mode:         "raw_file",
		RawPath:      params.requestRawPath,
		Bytes:        params.requestBytes,
		SHA256:       "",
		BodyType:     "",
		Keys:         nil,
		CaptureError: "",
	}
	responseRef := rawBodyReference{
		Mode:         "raw_file",
		RawPath:      params.responseRawPath,
		Bytes:        responseBytes,
		SHA256:       "",
		BodyType:     "",
		Keys:         nil,
		CaptureError: "",
	}
	return httpCaptureRecordInput{
		config:         params.cfg,
		policy:         params.capturePolicy,
		provider:       string(params.provider.ID()),
		upstreamURL:    "https://" + params.host + params.req.URL.RequestURI(),
		requestBody:    params.body,
		responseBody:   nil,
		requestIndex:   newCaptureBodyIndexFromReference(requestRef),
		responseIndex:  newCaptureBodyIndexFromReference(responseRef),
		responseLen:    responseBytes,
		duration:       time.Since(params.started),
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

func (p *Proxy) forwardAndCaptureProviderResponse(client *bufio.Writer, resp *http.Response, responseRawPath string) (int64, error) {
	var responseFile *os.File
	if responseRawPath != "" {
		var err error
		responseFile, err = os.OpenFile(responseRawPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
		if err != nil {
			p.log.Warn("mitm.provider.response.open_capture_failed", "path", responseRawPath, "err", err)
			return 0, fmt.Errorf("open raw cursor response: %w", err)
		}
		defer func() { _ = responseFile.Close() }()
	}

	chunked := resp.ContentLength < 0
	header := providerResponseHeader(resp, chunked)
	responseBytes, err := writeProviderResponseBytes(client, responseFile, header, "header")
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written, err := writeProviderResponseBodyChunk(client, responseFile, buf[:n], chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			written, err := writeProviderResponseEOF(client, responseFile, chunked)
			responseBytes += written
			if err != nil {
				return responseBytes, err
			}
			return responseBytes, nil
		}
		if readErr != nil {
			p.log.Warn("mitm.provider.response.read_body_failed", "err", readErr)
			return responseBytes, fmt.Errorf("read cursor response body: %w", readErr)
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

func writeProviderResponseBodyChunk(client *bufio.Writer, responseFile *os.File, chunk []byte, chunked bool) (int64, error) {
	var written int64
	if chunked {
		chunkHeader := fmt.Appendf(nil, "%x\r\n", len(chunk))
		count, err := writeProviderResponseBytes(client, responseFile, chunkHeader, "chunk header")
		written += count
		if err != nil {
			return written, err
		}
	}
	count, err := writeProviderResponseBytes(client, responseFile, chunk, "body")
	written += count
	if err != nil {
		return written, err
	}
	if chunked {
		count, err = writeProviderResponseString(client, responseFile, "\r\n", "chunk terminator")
		written += count
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func writeProviderResponseEOF(client *bufio.Writer, responseFile *os.File, chunked bool) (int64, error) {
	if !chunked {
		if err := client.Flush(); err != nil {
			slog.Warn("mitm.provider.response.flush_body_failed", "err", err)
			return 0, fmt.Errorf("flush cursor response body: %w", err)
		}
		return 0, nil
	}
	return writeProviderResponseString(client, responseFile, "0\r\n\r\n", "chunk EOF")
}

func writeProviderResponseBytes(client *bufio.Writer, responseFile *os.File, chunk []byte, label string) (int64, error) {
	if _, err := client.Write(chunk); err != nil {
		slog.Warn("mitm.provider.response.write_client_failed", "label", label, "err", err)
		return 0, fmt.Errorf("write cursor response %s: %w", label, err)
	}
	if responseFile != nil {
		if _, err := responseFile.Write(chunk); err != nil {
			slog.Warn("mitm.provider.response.capture_write_failed", "label", label, "err", err)
			return 0, fmt.Errorf("capture cursor response %s: %w", label, err)
		}
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.provider.response.flush_failed", "label", label, "err", err)
		return 0, fmt.Errorf("flush cursor response %s: %w", label, err)
	}
	return int64(len(chunk)), nil
}

func writeProviderResponseString(client *bufio.Writer, responseFile *os.File, text string, label string) (int64, error) {
	if _, err := client.WriteString(text); err != nil {
		slog.Warn("mitm.provider.response.write_client_failed", "label", label, "err", err)
		return 0, fmt.Errorf("write cursor response %s: %w", label, err)
	}
	if responseFile != nil {
		if _, err := responseFile.WriteString(text); err != nil {
			slog.Warn("mitm.provider.response.capture_write_failed", "label", label, "err", err)
			return 0, fmt.Errorf("capture cursor response %s: %w", label, err)
		}
	}
	if err := client.Flush(); err != nil {
		slog.Warn("mitm.provider.response.flush_failed", "label", label, "err", err)
		return 0, fmt.Errorf("flush cursor response %s: %w", label, err)
	}
	return int64(len(text)), nil
}
