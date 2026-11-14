package mitm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

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
func (p *Proxy) dispatchUpstream(
	upstreamCtx context.Context,
	writer http.ResponseWriter,
	req upstreamRequest,
) (*http.Response, bool) {
	upstreamURL := req.upstream + req.path
	upReq, err := http.NewRequestWithContext(
		upstreamCtx,
		req.method,
		upstreamURL,
		bytes.NewReader(req.body),
	)
	if err != nil {
		http.Error(writer, "build upstream request", http.StatusInternalServerError)

		return nil, false
	}

	copyHeaders(upReq.Header, req.header)
	upReq.Host = ""
	resp, err := p.httpClient.Do(upReq)
	if err != nil {
		p.log.WarnContext(
			upstreamCtx,
			"mitm.proxy.upstream_failed",
			"concern",
			"providers.mitm.errors",
			"provider",
			req.provider,
			"path",
			req.path,
			"err",
			err,
		)
		http.Error(writer, err.Error(), http.StatusBadGateway)

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
func (p *Proxy) registerPlainHTTP(
	ctx context.Context,
	cancel context.CancelFunc,
	writer http.ResponseWriter,
	request *http.Request,
	upstream string,
) (*livetrack.Session[TunnelMeta], func(string), bool) {
	reqSess, registerErr := p.Tunnels.Register(
		ctx,
		"mitm.http",
		TunnelMeta{
			ConnectHost:   request.Host,
			UpstreamAddr:  upstream,
			CaptureFile:   "",
			KeepaliveSeen: false,
		},
		&mitmHTTPCloser{cancel: cancel},
	)
	if registerErr != nil {
		cancel()
		p.log.WarnContext(
			ctx,
			"mitm.http.register_rejected",
			"concern",
			"providers.mitm.wire",
			"path",
			request.URL.Path,
			"err",
			registerErr,
		)
		http.Error(writer, "service draining", http.StatusServiceUnavailable)

		return nil, nil, false
	}

	release := func(reason string) {
		p.Tunnels.Release(ctx, reqSess, reason)
		cancel()
	}

	return reqSess, release, true
}

func (p *Proxy) recordPlainHTTPReadFailure(
	request *http.Request,
	cfg config.MITMConfig,
	provider string,
	upstreamURL string,
	started time.Time,
	err error,
) {
	p.recordHTTPFailure(
		request,
		http.Header{},
		buildHTTPFailureCaptureInput(
			cfg,
			provider,
			upstreamURL,
			nil,
			emptyCaptureBodyIndex(),
			emptyCaptureBodyIndex(),
			clock.Since(started),
			http.StatusBadRequest,
		),
		httpFailureRecord{
			includePayload:      false,
			includeUpstreamSend: false,
			errorCode:           "request_read_failed",
			errorMessage:        err.Error(),
		},
	)
}

func (p *Proxy) recordPlainHTTPDispatchFailure(
	request *http.Request,
	cfg config.MITMConfig,
	provider string,
	upstreamURL string,
	body []byte,
	requestIndex captureBodyIndex,
	started time.Time,
) {
	p.recordHTTPFailure(
		request,
		http.Header{},
		buildHTTPFailureCaptureInput(
			cfg,
			provider,
			upstreamURL,
			body,
			requestIndex,
			emptyCaptureBodyIndex(),
			clock.Since(started),
			http.StatusBadGateway,
		),
		httpFailureRecord{
			includePayload:      true,
			includeUpstreamSend: true,
			errorCode:           "upstream_dispatch_failed",
			errorMessage:        "upstream dispatch failed",
		},
	)
}

func (p *Proxy) handle(writer http.ResponseWriter, request *http.Request) {
	started := clock.Now()
	cfg := p.config()
	if request.Method == http.MethodConnect {
		p.handleConnect(writer, request)

		return
	}

	provider, upstream := classifyPlainRequest(request.URL)
	if provider == "" {
		http.Error(writer, "unsupported mitm route", http.StatusNotFound)

		return
	}

	if isWebsocketUpgrade(request) {
		p.handleWebsocket(writer, request, provider, upstream)

		return
	}

	p.handlePlainHTTP(writer, request, provider, upstream, started, cfg)
}

func (p *Proxy) handlePlainHTTP(
	writer http.ResponseWriter,
	request *http.Request,
	provider string,
	upstream string,
	started time.Time,
	cfg config.MITMConfig,
) {
	// CLYDE-324: upstream context shares request-scoped values with
	// r.Context() (trace ids, etc.) via [context.WithoutCancel] but
	// breaks the cancel chain so stdlib [http.Server] lifecycle changes
	// and HTTP keep-alive churn cannot abort the upstream HTTPS stream
	// mid-flight. Force-close from the registry cancels via the closer
	// installed in registerPlainHTTP; the deferred release also
	// cancels at handler return.
	upstreamCtx, upstreamCancel := context.WithCancel(context.WithoutCancel(request.Context()))
	plainHTTPSession, releasePlainHTTP, registered := p.registerPlainHTTP(
		upstreamCtx,
		upstreamCancel,
		writer,
		request,
		upstream,
	)
	if !registered {
		return
	}

	defer releasePlainHTTP("mitm.http.completed")

	body, err := io.ReadAll(request.Body)
	if err != nil {
		p.recordPlainHTTPReadFailure(
			request,
			cfg,
			provider,
			upstream+request.URL.RequestURI(),
			started,
			err,
		)
		http.Error(writer, "read request body", http.StatusBadRequest)

		return
	}

	_ = request.Body.Close()

	transforms := p.matchPlainHTTPHooks(provider, request, body)

	// Apply any request-body rewrite before indexing, dispatching, and capturing so
	// all three see the forwarded (possibly trimmed) body. Fail-open leaves body as-is.
	body, _ = p.transformRequestBody(upstreamCtx, transforms.RequestTransformer, body)
	requestBodyIndex := newCaptureBodyIndexFromSummary(summarizeBody(body))
	resp, dispatched := p.dispatchUpstream(
		upstreamCtx,
		writer,
		upstreamRequest{
			method:   request.Method,
			path:     request.URL.RequestURI(),
			header:   request.Header,
			body:     body,
			provider: provider,
			upstream: upstream,
		},
	)
	if !dispatched {
		p.recordPlainHTTPDispatchFailure(
			request,
			cfg,
			provider,
			upstream+request.URL.RequestURI(),
			body,
			requestBodyIndex,
			started,
		)

		return
	}

	defer func() { _ = resp.Body.Close() }()
	p.finishPlainHTTPResponse(
		upstreamCtx,
		writer,
		request,
		resp,
		transforms.Transformer,
		plainHTTPSession,
		plainHTTPCaptureFinalize{
			cfg:              cfg,
			provider:         provider,
			upstream:         upstream,
			requestBody:      body,
			capture:          nil,
			responseBytes:    0,
			requestBodyIndex: requestBodyIndex,
			duration:         0,
		},
		started,
	)
}

func (p *Proxy) matchPlainHTTPHooks(
	provider string,
	request *http.Request,
	body []byte,
) RequestResponseHookMatch {
	transformer, requestTransformer, err := p.matchRequestResponseHook(
		newRequestResponseHookRequest(
			provider,
			request.Host,
			request,
			newStaticRequestResponseHookBody(body),
			true,
		),
	)
	if err != nil {
		return RequestResponseHookMatch{
			Matched:            false,
			Transformer:        nil,
			RequestTransformer: nil,
			ContinueMatching:   false,
		}
	}

	return RequestResponseHookMatch{
		Matched:            transformer != nil || requestTransformer != nil,
		Transformer:        transformer,
		RequestTransformer: requestTransformer,
		ContinueMatching:   false,
	}
}

func (p *Proxy) finishPlainHTTPResponse(
	upstreamCtx context.Context,
	writer http.ResponseWriter,
	request *http.Request,
	resp *http.Response,
	transformer ResponseTransformer,
	plainHTTPSession *livetrack.Session[TunnelMeta],
	fin plainHTTPCaptureFinalize,
	started time.Time,
) {
	streamResp, err := p.applyResponseHook(upstreamCtx, transformer, resp)
	if err != nil {
		p.log.WarnContext(
			upstreamCtx,
			"mitm.proxy.response_hook_transform_failed",
			"concern",
			"providers.mitm.wire",
			"provider",
			fin.provider,
			"path",
			request.URL.Path,
			"err",
			err,
		)
		http.Error(writer, "response hook failed", http.StatusBadGateway)

		return
	}

	// Close the transformed body only when the hook produced a new response; when
	// no transform ran applyResponseHook returns resp unchanged, so the defer
	// above already covers it and a second close would double-close.
	if streamResp != resp {
		defer func() { _ = streamResp.Body.Close() }()
	}

	forwardResponseHeaders(writer.Header(), streamResp.Header)
	writer.WriteHeader(streamResp.StatusCode)
	// Buffer the response up to the store body cap so the full body reaches
	// the capture store while still streaming to the client. The wire-leg
	// summary is derived from this same buffer (summarizeBody previews/caps).
	captureBuffer := &limitedBuffer{limit: p.captureBodyCap(fin.cfg), buf: bytes.Buffer{}}
	counter := &countingWriter{writer: writer, bytesWritten: 0}
	flusher, _ := writer.(http.Flusher)
	copyErr := streamWithFlush(counter, captureBuffer, streamResp.Body, flusher, plainHTTPSession)
	duration := clock.Since(started)
	if copyErr != nil {
		p.log.Warn(
			"mitm.proxy.copy_failed",
			"concern",
			"providers.mitm.wire",
			"provider",
			fin.provider,
			"path",
			request.URL.Path,
			"err",
			copyErr,
		)
	}

	fin.capture = captureBuffer
	fin.responseBytes = counter.bytesWritten
	fin.duration = duration
	p.finalizePlainHTTPCapture(request, streamResp, fin)
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
	responseBytes    int64
	requestBodyIndex captureBodyIndex
	duration         time.Duration
}

// finalizePlainHTTPCapture decodes the buffered response, records the unified
// HTTP capture leg, persists the exchange to the SQLite capture store, appends
// the drift-capture record for native upstreams, and debounces a baseline
// refresh.
func (p *Proxy) finalizePlainHTTPCapture(
	request *http.Request,
	resp *http.Response,
	fin plainHTTPCaptureFinalize,
) {
	captureBody, decoded := decodeForCapture(
		fin.capture.Bytes(),
		resp.Header.Get("Content-Encoding"),
	)
	if decoded {
		p.log.Debug(
			"mitm.capture.decoded",
			"concern",
			"providers.mitm.wire",
			"provider",
			fin.provider,
			"path",
			request.URL.Path,
			"encoding",
			resp.Header.Get("Content-Encoding"),
			"raw_bytes",
			len(fin.capture.Bytes()),
			"decoded_bytes",
			len(captureBody),
		)
	}

	responseBodyIndex := newCaptureBodyIndexFromSummary(summarizeBody(captureBody))
	responseBodyLen := fin.responseBytes

	p.log.Info(
		"mitm.capture.completed",
		"concern",
		"providers.mitm.wire",
		"provider",
		fin.provider,
		"path",
		request.URL.Path,
		"status",
		resp.StatusCode,
		"duration_ms",
		fin.duration.Milliseconds(),
	)
	upstreamURL := fin.upstream + request.URL.RequestURI()
	p.recordHTTPCapture(
		request,
		resp.Header,
		httpCaptureRecordInput{
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
		},
	)
	p.recordCaptureStore(
		request,
		resp.Header,
		captureStoreInput{
			provider:        fin.provider,
			host:            request.Host,
			method:          request.Method,
			path:            request.URL.Path,
			status:          resp.StatusCode,
			requestBody:     fin.requestBody,
			responseBody:    captureBody,
			duration:        fin.duration,
			captureRules:    fin.cfg.CaptureRules,
			hasCaptureRules: true,
		},
	)
	p.recordDriftCapture(
		fin.cfg,
		driftCaptureInput{
			provider:    fin.provider,
			method:      request.Method,
			path:        request.URL.Path,
			upstreamURL: upstreamURL,
			header:      request.Header,
			body:        fin.requestBody,
		},
	)
	queueBaselineRefresh(request.Context(), p.store, fin.cfg, fin.provider, p.log)
}
