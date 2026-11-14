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

// upstreamRequest stores the method, path, headers, body, provider, and upstream
// address for one plain HTTP request.
type upstreamRequest struct {
	method   string
	path     string
	header   http.Header
	body     []byte
	provider string
	upstream string
}

// dispatchUpstream constructs and sends the upstream request with the context
// that handlePlainHTTP creates. It writes an HTTP error and returns false
// when request construction or the upstream send fails.
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

// registerPlainHTTP registers the exchange and its cancellation callback with
// the tunnel registry. Reload drains the registered exchange. Forced closure
// calls mitmHTTPCloser.Close to cancel the upstream request. The returned
// release function removes the exchange and cancels the upstream context.
// A draining registry rejects registration. This function cancels the context,
// writes HTTP 503, and returns false after that rejection.
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
	// CLYDE-324: context.WithoutCancel(request.Context()) preserves correlation
	// values and does not inherit request cancellation. HTTP server shutdown and
	// keep-alive changes must not cancel an active upstream stream.
	// context.WithCancel provides the callback that the tunnel registry uses
	// during forced closure. The deferred release cancels upstreamCtx when
	// handlePlainHTTP returns. A failed client write also ends this handler.
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

	// Rewrite the body before request indexing, forwarding, and capture.
	// A failed transformation preserves the original body.
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

	// handlePlainHTTP closes resp.Body after finishPlainHTTPResponse returns.
	// finishPlainHTTPResponse closes streamResp.Body when the hook creates a
	// different response.
	if streamResp != resp {
		defer func() { _ = streamResp.Body.Close() }()
	}

	forwardResponseHeaders(writer.Header(), streamResp.Header)
	writer.WriteHeader(streamResp.StatusCode)
	// Stream the response to the client and retain up to the capture store body
	// limit in captureBuffer. Capture storage and wire log summaries read that
	// buffer after streaming finishes.
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

// plainHTTPCaptureFinalize stores the request metadata and response results
// that finalizePlainHTTPCapture records after streaming finishes.
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

// finalizePlainHTTPCapture decodes the buffered response and records the HTTP
// capture leg. It then saves the exchange in SQLite, records drift data for
// native upstreams, and schedules the baseline refresh.
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
