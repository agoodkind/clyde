package mitm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logevent"
	codexprovider "goodkind.io/clyde/internal/providers/codex"
	"goodkind.io/clyde/internal/slogger"
)

var (
	anthropicUpstream = "https://api.anthropic.com"
	openAIUpstream    = "https://api.openai.com"
	chatGPTUpstream   = "https://chatgpt.com"
)

type Proxy struct {
	log         *slog.Logger
	client      *http.Client
	dialContext func(context.Context, string, string) (net.Conn, error)

	certMu sync.Mutex
	ca     *cursorCertAuthority

	cursorTLSClientConfig *tls.Config
	rawCaptureSeq         atomic.Uint64
	requestLog            *logevent.Emitter

	// Tunnels tracks every long-lived MITM connection (CONNECT
	// tunnels, intercepted Cursor TLS sessions, plain HTTP request
	// loops) so the daemon can drain or force-close them on reload
	// instead of relying on http.Server.Shutdown alone. See
	// internal/livetrack for the contract; the proxy installs each
	// session's closer to terminate hijacked client and upstream
	// connections.
	Tunnels *livetrack.Registry[TunnelMeta]

	// captureWriters owns this proxy's lumberjack rotated-writer pool
	// plus its flock files. Per-proxy ownership means daemon reload
	// closes the old proxy's writers and flocks deterministically; a
	// late tunnel goroutine that races shutdown will see the closed
	// cache and abort instead of re-creating the writer (CLYDE-299).
	captureWriters *captureWriterCache

	mu       sync.RWMutex
	cfg      config.MITMConfig
	base     string
	listener net.Listener
	server   *http.Server
}

// NewProxy constructs a Proxy bound to the supplied listener. The
// caller (the daemon) owns listener lifecycle; the proxy serves on
// it until Shutdown returns. Callers must invoke Serve to start
// accepting requests.
//
// The logging argument carries the typed request-emitter configuration
// (required-leg overrides and incomplete-policy) the MITM emitter applies. A
// zero-value [config.LoggingRequest] yields default required legs and the warn
// policy, matching the adapter's behavior.
func NewProxy(cfg config.MITMConfig, logging config.LoggingRequest, log *slog.Logger, listener net.Listener) (*Proxy, error) {
	if listener == nil {
		return nil, fmt.Errorf("mitm: listener is required")
	}
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernProviderMITMLifecycle)
	ca, err := loadOrCreateCertAuthority(cfg.CA.CertPath, cfg.CA.KeyPath, time.Now)
	if err != nil {
		log.Warn("mitm.cursor.tls.ca_load_failed",
			"cert_path", cfg.CA.CertPath,
			"key_path", cfg.CA.KeyPath,
			"err", err,
		)
		return nil, fmt.Errorf("load cursor mitm ca: %w", err)
	}
	p := &Proxy{
		log:                   log.With("component", "mitm"),
		client:                http.DefaultClient,
		dialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:                sync.Mutex{},
		ca:                    ca,
		cursorTLSClientConfig: nil,
		rawCaptureSeq:         atomic.Uint64{},
		requestLog: logevent.NewEmitter(
			slogger.WithConcern(log, slogger.ConcernProviderMITMWire),
			logevent.RequiredLegsFromStrings(logging.RequiredLegs),
			mitmEmitterOptions(logging)...,
		),
		Tunnels: livetrack.New[TunnelMeta](livetrack.Options[TunnelMeta]{
			Component:     "mitm",
			Concern:       slogger.ConcernProviderMITMLifecycle,
			Log:           log,
			PollEvery:     50 * time.Millisecond,
			CloserGrace:   2 * time.Second,
			ParallelClose: false,
			Now:           nil,
		}),
		captureWriters: newCaptureWriterCache(log),
		mu:             sync.RWMutex{},
		cfg:            cfg,
		base:           "http://" + listener.Addr().String(),
		listener:       listener,
		server:         nil,
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.handle)}
	return p, nil
}

// mitmEmitterOptions converts the typed [config.LoggingRequest] into the
// [logevent.EmitterOption] set the MITM request emitter uses. Production
// callers wire a nil test-fail handler; tests wire a non-nil handler to fail
// the active test under [logevent.IncompletePolicyFailTest].
func mitmEmitterOptions(request config.LoggingRequest) []logevent.EmitterOption {
	policy, ok := logevent.ParseIncompletePolicy(request.IncompletePolicy)
	if !ok {
		policy = logevent.IncompletePolicyWarn
	}
	return []logevent.EmitterOption{
		logevent.WithIncompletePolicy(policy),
		logevent.WithTestingTB(nil),
	}
}

// Serve runs the proxy's HTTP server on its bound listener. It blocks
// until Shutdown is called or the listener returns an unrecoverable
// error.
func (p *Proxy) Serve() error {
	if p.listener == nil {
		return fmt.Errorf("mitm: proxy has no listener")
	}
	p.log.Info("mitm.proxy.started",
		"base_url", p.base,
		"capture_dir", p.cfg.CaptureDir,
		"providers", p.cfg.Providers,
		"raw_capture_enabled", p.cfg.RawCaptureEnabled,
	)
	if err := p.server.Serve(p.listener); err != nil && err != http.ErrServerClosed {
		p.log.Error("mitm.proxy.serve_failed", "err", err)
		return fmt.Errorf("mitm serve: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the proxy's HTTP server, drains
// registered tunnels until ctx is canceled, and closes the per-proxy
// capture writer cache so the replacement daemon can rebind. The
// Cloudflare keepalive case (api2.cursor.sh CONNECT tunnels that
// never close on their own) is the empirical reason this is no longer
// a bare [http.Server.Shutdown]: the registry's force-close path
// terminates wedged tunnels under the configured grace, and the
// writer-cache close releases the JSONL flock and locks out late
// tunnel goroutines from re-creating a fresh writer on the same path
// (CLYDE-299). Idempotent: multiple Shutdown calls are safe.
func (p *Proxy) Shutdown(ctx context.Context) error {
	if p.server == nil {
		return nil
	}
	if p.Tunnels == nil || p.Tunnels.Count() == 0 {
		if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("close mitm listener: %w", err)
		}
		p.closeCaptureWriters()
		return nil
	}
	httpErr := p.server.Shutdown(ctx)
	if httpErr != nil {
		p.log.WarnContext(ctx, "mitm.proxy.http_shutdown_failed", "err", httpErr)
	}
	if p.Tunnels != nil && (httpErr != nil || p.Tunnels.Count() > 0) {
		result := p.Tunnels.Drain(ctx, "mitm.shutdown")
		p.log.InfoContext(ctx, "mitm.proxy.tunnels_drained",
			"final", result.Final.String(),
			"remaining", result.Remaining,
			"force_closed", result.ForceClosed,
			"duration_ms", result.Duration.Milliseconds(),
		)
	}
	p.closeCaptureWriters()
	if httpErr != nil {
		return fmt.Errorf("mitm shutdown: %w", httpErr)
	}
	return nil
}

// closeCaptureWriters drains and closes the per-proxy capture writer
// cache. Safe on a nil cache so test fixtures that build a *Proxy by
// hand without going through NewProxy do not crash on Shutdown.
func (p *Proxy) closeCaptureWriters() {
	if p.captureWriters == nil {
		return
	}
	p.captureWriters.close()
}

// SetConfig updates the proxy's runtime config. The daemon calls
// this on config reload so capture directory, raw capture, and provider set
// changes react without rebinding the listener.
func (p *Proxy) SetConfig(cfg config.MITMConfig) {
	p.setConfig(cfg)
}

func (p *Proxy) setConfig(cfg config.MITMConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg
}

func (p *Proxy) config() config.MITMConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// BaseURL returns the loopback HTTP URL clients should use to reach the
// daemon-owned MITM proxy.
func (p *Proxy) BaseURL() string { return p.base }

func (p *Proxy) ClaudeBaseURL() string { return p.BaseURL() }

// prepareCaptureWriters returns the per-request capture state. When raw
// capture is enabled, it primes a sidecar response writer; otherwise it
// returns a filtered inline summary index and nil writer.
func (p *Proxy) prepareCaptureWriters(cfg config.MITMConfig, provider string, path string, body []byte) (captureBodyIndex, *failOpenRawCaptureWriter, error) {
	requestBodyIndex := newCaptureBodyIndexFromSummary(summarizeBody(body))
	if !cfg.RawCaptureEnabled {
		return requestBodyIndex, nil, nil
	}
	rawSetup := p.prepareRawHTTPCapture(cfg, provider, path, body)
	return rawSetup.requestBodyIndex, rawSetup.responseRawWriter, rawSetup.responseRawError
}

// upstreamRequest captures the inbound request fields the proxy
// forwards to the upstream HTTPS endpoint. Splitting the inbound
// [http.Request] from the upstream request lets the upstream call
// run on its own context (decoupled from r.Context() to survive
// stdlib http.Server lifecycle cancellations; CLYDE-324) without
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
// the cancel chain, so stdlib http.Server lifecycle transitions and
// HTTP keep-alive churn do not abort the upstream stream mid-flight.
// Force-close from the registry's [mitmHTTPCloser] cancels upstreamCtx
// directly. Genuine client disconnect surfaces via streamWithFlush's
// failed write, which fires the deferred release in handle (CLYDE-324).
func (p *Proxy) dispatchUpstream(upstreamCtx context.Context, w http.ResponseWriter, req upstreamRequest) (*http.Response, bool) {
	upstreamURL := req.upstream + req.path
	upReq, err := http.NewRequestWithContext(upstreamCtx, req.method, upstreamURL, bytes.NewReader(req.body))
	if err != nil {
		http.Error(w, "build upstream request", http.StatusInternalServerError)
		return nil, false
	}
	copyHeaders(upReq.Header, req.header)
	upReq.Host = ""
	resp, err := p.client.Do(upReq)
	if err != nil {
		p.log.WarnContext(upstreamCtx, "mitm.proxy.upstream_failed", "provider", req.provider, "path", req.path, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
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
// http.Server lifecycle on shutdown and by HTTP keep-alive churn,
// and that cancellation would silently abort the in-flight upstream
// HTTPS request to api.anthropic.com mid-stream. ctx is cancelled
// only by the registry's force-close path (via [mitmHTTPCloser.Close],
// which calls cancel) or by the deferred release at the end of the
// handler. Genuine client disconnect surfaces via streamWithFlush's
// failed write, which fires the deferred release naturally.
func (p *Proxy) registerPlainHTTP(ctx context.Context, cancel context.CancelFunc, w http.ResponseWriter, r *http.Request, upstream string) (func(string), bool) {
	reqSess, registerErr := p.Tunnels.Register(ctx, "mitm.http", TunnelMeta{
		ConnectHost:   r.Host,
		UpstreamAddr:  upstream,
		CaptureFile:   "",
		KeepaliveSeen: false,
	}, &mitmHTTPCloser{cancel: cancel})
	if registerErr != nil {
		cancel()
		p.log.WarnContext(ctx, "mitm.http.register_rejected", "path", r.URL.Path, "err", registerErr)
		http.Error(w, "service draining", http.StatusServiceUnavailable)
		return nil, false
	}
	release := func(reason string) {
		p.Tunnels.Release(ctx, reqSess, reason)
		cancel()
	}
	return release, true
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	started := currentTime()
	cfg := p.config()
	capturePolicy := captureFilePolicyFromConfig(cfg)
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	provider, upstream := classifyRoute(r.URL.Path)
	if provider == "" {
		http.Error(w, "unsupported mitm route", http.StatusNotFound)
		return
	}
	if isWebsocketUpgrade(r) {
		p.handleWebsocket(w, r, provider, upstream)
		return
	}
	// CLYDE-324: upstream context shares request-scoped values with
	// r.Context() (trace ids, etc.) via [context.WithoutCancel] but
	// breaks the cancel chain so stdlib http.Server lifecycle changes
	// and HTTP keep-alive churn cannot abort the upstream HTTPS stream
	// mid-flight. Force-close from the registry cancels via the closer
	// installed in registerPlainHTTP; the deferred release also
	// cancels at handler return.
	upstreamCtx, upstreamCancel := context.WithCancel(context.WithoutCancel(r.Context()))
	releasePlainHTTP, ok := p.registerPlainHTTP(upstreamCtx, upstreamCancel, w, r, upstream)
	if !ok {
		return
	}
	defer releasePlainHTTP("mitm.http.completed")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	requestBodyIndex, responseRawWriter, responseRawError := p.prepareCaptureWriters(cfg, provider, r.URL.Path, body)
	resp, ok := p.dispatchUpstream(upstreamCtx, w, upstreamRequest{
		method:   r.Method,
		path:     r.URL.RequestURI(),
		header:   r.Header,
		body:     body,
		provider: provider,
		upstream: upstream,
	})
	if !ok {
		return
	}
	defer func() { _ = resp.Body.Close() }()

	forwardResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	capture := &limitedBuffer{limit: 16 * 1024}
	captureWriter := io.Writer(capture)
	if responseRawWriter != nil {
		captureWriter = io.MultiWriter(capture, responseRawWriter)
	}
	flusher, _ := w.(http.Flusher)
	copyErr := streamWithFlush(w, captureWriter, resp.Body, flusher)
	if responseRawWriter != nil {
		responseRawWriter.Close()
	}
	duration := time.Since(started)
	if copyErr != nil {
		p.log.Warn("mitm.proxy.copy_failed", "provider", provider, "path", r.URL.Path, "err", copyErr)
	}
	captureBody, decoded := decodeForCapture(capture.Bytes(), resp.Header.Get("Content-Encoding"))
	if decoded {
		p.log.Debug("mitm.capture.decoded",
			"provider", provider,
			"path", r.URL.Path,
			"encoding", resp.Header.Get("Content-Encoding"),
			"raw_bytes", len(capture.Bytes()),
			"decoded_bytes", len(captureBody),
		)
	}
	responseBodyIndex, responseBodyLen := responseCaptureIndex(cfg.RawCaptureEnabled, captureBody, responseRawWriter, responseRawError)

	p.log.Info("mitm.capture.completed",
		"provider", provider,
		"path", r.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", duration.Milliseconds(),
	)
	p.recordHTTPCapture(r, resp.Header, httpCaptureRecordInput{
		config:         cfg,
		policy:         capturePolicy,
		provider:       provider,
		upstreamURL:    upstream + r.URL.RequestURI(),
		requestBody:    body,
		responseBody:   captureBody,
		requestIndex:   requestBodyIndex,
		responseIndex:  responseBodyIndex,
		responseLen:    responseBodyLen,
		duration:       duration,
		responseStatus: resp.StatusCode,
	})
	queueBaselineRefresh(r.Context(), cfg, provider, p.log)
}

func forwardResponseHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "content-length", "transfer-encoding", "connection":
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type httpCaptureRecordInput struct {
	config         config.MITMConfig
	policy         CaptureFilePolicy
	provider       string
	upstreamURL    string
	requestBody    []byte
	responseBody   []byte
	requestIndex   captureBodyIndex
	responseIndex  captureBodyIndex
	responseLen    int64
	duration       time.Duration
	responseStatus int
}

type captureBodyIndex struct {
	raw json.RawMessage
}

type captureBodySummary struct {
	Mode     string   `json:"mode,omitempty"`
	BodyType string   `json:"body_type,omitempty"`
	Bytes    int      `json:"bytes,omitempty"`
	SHA256   string   `json:"sha256,omitempty"`
	Keys     []string `json:"keys,omitempty"`
	Messages int      `json:"messages,omitempty"`
	Input    int      `json:"input,omitempty"`
	Tools    int      `json:"tools,omitempty"`
	Model    string   `json:"model,omitempty"`
	ArrayLen int      `json:"array_len,omitempty"`
	Preview  string   `json:"preview,omitempty"`
}

func newCaptureBodyIndexFromSummary(summary captureBodySummary) captureBodyIndex {
	raw, err := json.Marshal(summary)
	if err != nil {
		raw = json.RawMessage(`null`)
	}
	return captureBodyIndex{raw: raw}
}

func newCaptureBodyIndexFromReference(reference rawBodyReference) captureBodyIndex {
	raw, err := json.Marshal(reference)
	if err != nil {
		raw = json.RawMessage(`null`)
	}
	return captureBodyIndex{raw: raw}
}

type rawHTTPCaptureSetup struct {
	requestBodyIndex  captureBodyIndex
	responseRawWriter *failOpenRawCaptureWriter
	responseRawError  error
}

func (p *Proxy) prepareRawHTTPCapture(cfg config.MITMConfig, provider string, path string, body []byte) rawHTTPCaptureSetup {
	rawSetup := rawHTTPCaptureSetup{
		requestBodyIndex:  newCaptureBodyIndexFromReference(rawBodyReferenceFromBytes(body, "", 0, fmt.Errorf("raw request sidecar was not prepared"))),
		responseRawWriter: nil,
		responseRawError:  nil,
	}
	requestRawPath, responseRawPath, err := p.nextHTTPCapturePaths(cfg.CaptureDir, provider, path)
	if err != nil {
		p.log.Warn("mitm.capture.raw_paths_failed", "provider", provider, "path", path, "err", err)
		rawSetup.responseRawError = err
		return rawSetup
	}
	requestBytes, err := writeRawCaptureFile(requestRawPath, func(dst io.Writer) error {
		_, writeErr := dst.Write(body)
		return writeErr
	})
	rawSetup.requestBodyIndex = newCaptureBodyIndexFromReference(rawBodyReferenceFromBytes(body, requestRawPath, requestBytes, err))
	if err != nil {
		p.log.Warn("mitm.capture.raw_request_failed", "provider", provider, "path", path, "err", err)
	}
	responseFile, err := os.OpenFile(responseRawPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, rawCaptureFileMode)
	if err != nil {
		p.log.Warn("mitm.capture.raw_response_open_failed", "provider", provider, "path", path, "raw_path", responseRawPath, "err", err)
		rawSetup.responseRawError = err
		return rawSetup
	}
	rawSetup.responseRawWriter = newFailOpenRawCaptureWriter(p.log, responseRawPath, responseFile)
	return rawSetup
}

func responseCaptureIndex(
	rawCaptureEnabled bool,
	captureBody []byte,
	responseRawWriter *failOpenRawCaptureWriter,
	responseRawError error,
) (captureBodyIndex, int64) {
	if !rawCaptureEnabled {
		return newCaptureBodyIndexFromSummary(summarizeBody(captureBody)), int64(len(captureBody))
	}
	if responseRawWriter != nil {
		return newCaptureBodyIndexFromReference(responseRawWriter.Reference()), responseRawWriter.Count()
	}
	if responseRawError == nil {
		responseRawError = fmt.Errorf("raw response sidecar was not prepared")
	}
	ref := rawBodyReferenceFromBytes(captureBody, "", int64(len(captureBody)), responseRawError)
	return newCaptureBodyIndexFromReference(ref), int64(len(captureBody))
}

func (p *Proxy) recordHTTPCapture(r *http.Request, responseHeader http.Header, input httpCaptureRecordInput) {
	recorder := p.beginHTTPLogRecorder(r, input)
	ctx := r.Context()
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMIngress, logevent.PhaseStarted, input)
	p.emitHTTPPayloadLeg(ctx, recorder, r, responseHeader, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMUpstreamSend, logevent.PhaseStarted, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMUpstreamStart, logevent.PhaseCompleted, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMForward, logevent.PhaseCompleted, input)
	p.emitHTTPLogLeg(ctx, recorder, logevent.LegMITMCaptureIndex, logevent.PhaseCompleted, input)
	p.completeHTTPLogRecorder(ctx, recorder, input)
}

func classifyRoute(path string) (provider string, upstream string) {
	switch {
	case strings.HasPrefix(path, "/v1/messages"), strings.HasPrefix(path, "/v1/models"):
		return "claude", anthropicUpstream
	case strings.HasPrefix(path, "/backend-api/"):
		return "codex", chatGPTUpstream
	case strings.HasPrefix(path, "/v1/"):
		return "codex", openAIUpstream
	default:
		return "", ""
	}
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type rawBodyReference struct {
	Mode         string   `json:"mode"`
	RawPath      string   `json:"raw_path,omitempty"`
	Bytes        int64    `json:"bytes"`
	SHA256       string   `json:"sha256,omitempty"`
	BodyType     string   `json:"body_type,omitempty"`
	Keys         []string `json:"keys,omitempty"`
	CaptureError string   `json:"capture_error,omitempty"`
}

func rawBodyReferenceFromBytes(body []byte, rawPath string, byteCount int64, err error) rawBodyReference {
	ref := rawBodyReference{
		Mode:         "raw_file",
		RawPath:      rawPath,
		Bytes:        byteCount,
		SHA256:       sha256Hex(body),
		BodyType:     bodyTypeForCapture(body),
		Keys:         bodyKeysForCapture(body),
		CaptureError: "",
	}
	if err != nil {
		ref.CaptureError = err.Error()
	}
	return ref
}

func bodyTypeForCapture(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "empty"
	}
	switch trimmed[0] {
	case '{':
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return "json_object"
		}
	case '[':
		var decoded []json.RawMessage
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return "json_array"
		}
	}
	var scalar json.RawMessage
	if err := json.Unmarshal(trimmed, &scalar); err != nil {
		return "bytes"
	}
	return "json_scalar"
}

func bodyKeysForCapture(body []byte) []string {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &decoded); err != nil {
		return nil
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

type failOpenRawCaptureWriter struct {
	log    *slog.Logger
	path   string
	file   *os.File
	hash   hash.Hash
	count  int64
	failed bool
}

func newFailOpenRawCaptureWriter(log *slog.Logger, path string, file *os.File) *failOpenRawCaptureWriter {
	return &failOpenRawCaptureWriter{
		log:    log,
		path:   path,
		file:   file,
		hash:   sha256.New(),
		count:  0,
		failed: false,
	}
}

func (w *failOpenRawCaptureWriter) Write(chunk []byte) (int, error) {
	_, _ = w.hash.Write(chunk)
	if w.failed {
		return len(chunk), nil
	}
	n, err := w.file.Write(chunk)
	w.count += int64(n)
	if err != nil {
		w.failed = true
		w.log.Warn("mitm.capture.raw_response_write_failed", "raw_path", w.path, "err", err)
		return len(chunk), nil
	}
	return len(chunk), nil
}

func (w *failOpenRawCaptureWriter) Close() {
	if err := w.file.Close(); err != nil {
		w.failed = true
		w.log.Warn("mitm.capture.raw_response_close_failed", "raw_path", w.path, "err", err)
	}
}

func (w *failOpenRawCaptureWriter) Count() int64 {
	return w.count
}

func (w *failOpenRawCaptureWriter) Reference() rawBodyReference {
	ref := rawBodyReference{
		Mode:         "raw_file",
		RawPath:      w.path,
		Bytes:        w.count,
		SHA256:       hex.EncodeToString(w.hash.Sum(nil)),
		BodyType:     "",
		Keys:         nil,
		CaptureError: "",
	}
	if w.failed {
		ref.CaptureError = "raw response sidecar write failed"
	}
	return ref
}

func summarizeBody(body []byte) captureBodySummary {
	summary := captureBodySummary{
		Mode:     "filtered_inline",
		BodyType: bodyTypeForCapture(body),
		Bytes:    len(body),
		SHA256:   sha256Hex(body),
		Keys:     nil,
		Messages: 0,
		Input:    0,
		Tools:    0,
		Model:    "",
		ArrayLen: 0,
		Preview:  "",
	}
	return summarizeJSON(body, summary)
}

func summarizeJSON(body []byte, summary captureBodySummary) captureBodySummary {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return summary
	}
	if summary.BodyType == "bytes" {
		if len(trimmed) > 512 {
			summary.Preview = trimmed[:512]
		} else {
			summary.Preview = trimmed
		}
		return summary
	}
	if summary.BodyType == "json_array" {
		var values []json.RawMessage
		if err := json.Unmarshal(body, &values); err == nil {
			summary.ArrayLen = len(values)
		}
		return summary
	}
	if summary.BodyType != "json_object" {
		return summary
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return summary
	}
	summary.Keys = bodyKeysForCapture(body)
	summary.Messages = rawFieldArrayLen(fields, "messages")
	summary.Input = rawFieldArrayLen(fields, "input")
	summary.Tools = rawFieldArrayLen(fields, "tools")
	summary.Model = rawFieldString(fields, "model")
	return summary
}

func rawFieldArrayLen(fields map[string]json.RawMessage, key string) int {
	raw, ok := fields[key]
	if !ok {
		return 0
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return 0
	}
	return len(values)
}

func rawFieldString(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// writeCaptureLine appends one JSONL capture record through this
// proxy's writer cache. After Shutdown, rotated writes return
// ErrCaptureSinkClosed (CLYDE-299).
func (p *Proxy) writeCaptureLine(dir string, line []byte, policy CaptureFilePolicy) error {
	if p.captureWriters == nil {
		return fmt.Errorf("mitm: proxy has no capture writer cache")
	}
	return p.captureWriters.writeLine(dir, line, policy)
}

type limitedBuffer struct {
	limit int
	buf   bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remain := b.limit - b.buf.Len()
	if remain > 0 {
		if len(p) > remain {
			p = p[:remain]
		}
		_, _ = b.buf.Write(p)
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

// streamWithFlush copies upstream response bytes to the client and
// the capture buffer in chunks, flushing after each successful read
// so SSE deltas reach the client in real time. Without the per-read
// flush, Go's http.Server buffers up to its internal threshold and
// stream consumers (claude-cli, Cursor) see batched deltas or hang
// waiting for the first byte.
func streamWithFlush(client io.Writer, capture io.Writer, src io.Reader, flusher http.Flusher) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := client.Write(chunk); werr != nil {
				return werr
			}
			_, _ = capture.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// expandHome rewrites a leading "~" or "~/" in a path to the user's
// home directory. Go's os.MkdirAll and os.OpenFile do not perform
// shell-style tilde expansion, and TOML configs frequently use "~"
// as a portable home marker. This helper closes that gap for the
// capture_dir setting and any other path the proxy reads.
func expandHome(path string) string {
	if path == "" {
		return path
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// ClaudeEnv returns the env overrides Claude CLI needs to route
// through the daemon-owned MITM proxy. The proxy must already be
// running; callers pass the daemon-owned instance. If MITM is
// disabled by config, returns nil so callers can skip env injection
// entirely.
func ClaudeEnv(_ context.Context, cfg config.MITMConfig, proxy *Proxy) (map[string]string, error) {
	if !cfg.EnabledDefault || !cfg.EnabledFor("claude") {
		return nil, nil
	}
	if proxy == nil {
		return nil, fmt.Errorf("mitm: proxy is not running")
	}
	return map[string]string{"ANTHROPIC_BASE_URL": proxy.ClaudeBaseURL()}, nil
}

// CodexEnv returns the env overrides Codex CLI needs to route through the
// daemon-owned MITM proxy. Codex talks to HTTPS and CONNECT targets, so the
// daemon advertises a standard loopback proxy URL rather than a provider-
// specific base URL override.
func CodexEnv(_ context.Context, cfg config.MITMConfig, proxy *Proxy) (map[string]string, error) {
	if !cfg.EnabledDefault || !cfg.EnabledFor("codex") {
		return nil, nil
	}
	if proxy == nil {
		return nil, fmt.Errorf("mitm: proxy is not running")
	}
	env := make(map[string]string, len(codexProxyEnvKeys()))
	for _, key := range codexProxyEnvKeys() {
		env[key] = proxy.BaseURL()
	}
	return env, nil
}

func codexProxyEnvKeys() []string {
	return []string{
		codexprovider.HTTPProxyEnv,
		codexprovider.HTTPSProxyEnv,
		codexprovider.AllProxyEnv,
		"http_proxy",
		"https_proxy",
		"all_proxy",
	}
}

type CodexOverlay struct {
	Home string
}
