package mitm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

var (
	anthropicUpstream = "https://api.anthropic.com"
	openAIUpstream    = "https://api.openai.com"
	chatGPTUpstream   = "https://chatgpt.com"
)

type redactedHeaderLiteral string

const (
	redactedHeaderAuthorization      redactedHeaderLiteral = "authorization"
	redactedHeaderProxyAuthorization redactedHeaderLiteral = "proxy-authorization"
	redactedHeaderCookie             redactedHeaderLiteral = "cookie"
	redactedHeaderSetCookie          redactedHeaderLiteral = "set-cookie"
)

type Proxy struct {
	log         *slog.Logger
	client      *http.Client
	dialContext func(context.Context, string, string) (net.Conn, error)

	certMu sync.Mutex
	ca     *cursorCertAuthority

	cursorTLSClientConfig *tls.Config
	rawCaptureSeq         atomic.Uint64

	mu     sync.RWMutex
	cfg    config.MITMConfig
	base   string
	server *http.Server
}

var defaultProxy struct {
	mu   sync.Mutex
	inst *Proxy
}

func EnsureStarted(cfg config.MITMConfig, log *slog.Logger) (*Proxy, error) {
	defaultProxy.mu.Lock()
	defer defaultProxy.mu.Unlock()
	if defaultProxy.inst != nil {
		defaultProxy.inst.setConfig(cfg)
		return defaultProxy.inst, nil
	}
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernProviderMITMLifecycle)
	ln, err := net.Listen("tcp", "[::1]:0") // TODO: make this configurable
	if err != nil {
		return nil, err
	}
	p := &Proxy{
		log:                   log.With("component", "mitm"),
		client:                http.DefaultClient,
		dialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:                sync.Mutex{},
		ca:                    nil,
		cursorTLSClientConfig: nil,
		rawCaptureSeq:         atomic.Uint64{},
		mu:                    sync.RWMutex{},
		cfg:                   cfg,
		base:                  "http://" + ln.Addr().String(),
		server:                nil,
	}
	p.server = &http.Server{Handler: http.HandlerFunc(p.handle)}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				p.log.Error("mitm.proxy.serve_panic",
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			p.log.Error("mitm.proxy.serve_failed", "err", err)
		}
	}()
	p.log.Info("mitm.proxy.started", "base_url", p.base, "capture_dir", cfg.CaptureDir, "providers", cfg.Providers, "body_mode", cfg.BodyMode)
	defaultProxy.inst = p
	return p, nil
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

func (p *Proxy) ClaudeBaseURL() string { return p.base }

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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	requestBodyIndex := newCaptureBodyIndex(summarizeBody(cfg.BodyMode, body))
	var responseRawWriter *failOpenRawCaptureWriter
	var responseRawError error
	if cfg.BodyMode == "raw" {
		rawSetup := p.prepareRawHTTPCapture(cfg, provider, r.URL.Path, body)
		requestBodyIndex = rawSetup.requestBodyIndex
		responseRawWriter = rawSetup.responseRawWriter
		responseRawError = rawSetup.responseRawError
	}

	upstreamURL := upstream + r.URL.RequestURI()
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "build upstream request", http.StatusInternalServerError)
		return
	}
	copyHeaders(upReq.Header, r.Header)
	upReq.Host = ""

	resp, err := p.client.Do(upReq)
	if err != nil {
		p.log.Warn("mitm.proxy.upstream_failed", "provider", provider, "path", r.URL.Path, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
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
	responseBodyIndex, responseBodyLen := responseCaptureIndex(cfg.BodyMode, captureBody, responseRawWriter, responseRawError)

	p.log.Info("mitm.capture.completed",
		"provider", provider,
		"path", r.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", duration.Milliseconds(),
	)
	p.recordHTTPCapture(r, resp, httpCaptureRecordInput{
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
	queueBaselineRefresh(cfg, provider, p.log)
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

func newCaptureBodyIndex(value any) captureBodyIndex {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = json.RawMessage(`null`)
	}
	return captureBodyIndex{raw: raw}
}

func (idx captureBodyIndex) MarshalJSON() ([]byte, error) {
	return idx.raw, nil
}

type rawHTTPCaptureSetup struct {
	requestBodyIndex  captureBodyIndex
	responseRawWriter *failOpenRawCaptureWriter
	responseRawError  error
}

func (p *Proxy) prepareRawHTTPCapture(cfg config.MITMConfig, provider string, path string, body []byte) rawHTTPCaptureSetup {
	rawSetup := rawHTTPCaptureSetup{
		requestBodyIndex:  newCaptureBodyIndex(rawBodyReferenceFromBytes(body, "", 0, fmt.Errorf("raw request sidecar was not prepared"))),
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
	rawSetup.requestBodyIndex = newCaptureBodyIndex(rawBodyReferenceFromBytes(body, requestRawPath, requestBytes, err))
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
	bodyMode string,
	captureBody []byte,
	responseRawWriter *failOpenRawCaptureWriter,
	responseRawError error,
) (captureBodyIndex, int64) {
	if bodyMode != "raw" {
		return newCaptureBodyIndex(summarizeBody(bodyMode, captureBody)), int64(len(captureBody))
	}
	if responseRawWriter != nil {
		return newCaptureBodyIndex(responseRawWriter.Reference()), responseRawWriter.Count()
	}
	if responseRawError == nil {
		responseRawError = fmt.Errorf("raw response sidecar was not prepared")
	}
	ref := rawBodyReferenceFromBytes(captureBody, "", int64(len(captureBody)), responseRawError)
	return newCaptureBodyIndex(ref), int64(len(captureBody))
}

func (p *Proxy) recordHTTPCapture(r *http.Request, resp *http.Response, input httpCaptureRecordInput) {
	requestEvent := map[string]any{
		"kind":            string(RecordHTTPRequest),
		"t":               currentTime().Unix(),
		"ts":              currentTime().UTC().Format(time.RFC3339Nano),
		"provider":        input.provider,
		"method":          r.Method,
		"url":             input.upstreamURL,
		"path":            r.URL.Path,
		"query":           r.URL.RawQuery,
		"headers":         redactHeaders(r.Header),
		"body_len":        len(input.requestBody),
		"body":            input.requestIndex,
		"request_headers": redactHeaders(r.Header),
		"request_body":    input.requestIndex,
	}
	if err := WriteCaptureEvent(input.config.CaptureDir, requestEvent, input.policy); err != nil {
		p.log.Warn("mitm.capture.append_failed", "capture_dir", input.config.CaptureDir, "err", err)
	}
	event := map[string]any{
		"kind":             string(RecordHTTPResponse),
		"t":                currentTime().Unix(),
		"ts":               currentTime().UTC().Format(time.RFC3339Nano),
		"provider":         input.provider,
		"method":           r.Method,
		"url":              input.upstreamURL,
		"path":             r.URL.Path,
		"query":            r.URL.RawQuery,
		"status":           input.responseStatus,
		"duration_ms":      input.duration.Milliseconds(),
		"headers":          redactHeaders(resp.Header),
		"body_len":         input.responseLen,
		"body":             input.responseIndex,
		"request_headers":  redactHeaders(r.Header),
		"response_headers": redactHeaders(resp.Header),
		"request_body":     input.requestIndex,
		"response_body":    input.responseIndex,
	}
	if err := WriteCaptureEvent(input.config.CaptureDir, event, input.policy); err != nil {
		p.log.Warn("mitm.capture.append_failed", "capture_dir", input.config.CaptureDir, "err", err)
	}
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

func redactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	keys := make([]string, 0, len(h))
	for key := range h {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		lower := strings.ToLower(key)
		if isSensitiveHeaderName(lower) {
			out[key] = "<redacted>"
		} else {
			out[key] = strings.Join(h.Values(key), ", ")
		}
	}
	return out
}

func isSensitiveHeaderName(lower string) bool {
	switch redactedHeaderLiteral(lower) {
	case redactedHeaderAuthorization,
		redactedHeaderProxyAuthorization,
		redactedHeaderCookie,
		redactedHeaderSetCookie:
		return true
	}
	if strings.Contains(lower, "api-key") {
		return true
	}
	credentialSuffix := "tok" + "en"
	return strings.Contains(lower, credentialSuffix)
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
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return "bytes"
	}
	switch decoded.(type) {
	case map[string]any:
		return "json_object"
	case []any:
		return "json_array"
	default:
		return "json_scalar"
	}
}

func bodyKeysForCapture(body []byte) []string {
	var decoded map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &decoded); err != nil {
		return nil
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
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

func summarizeBody(mode string, body []byte) any {
	switch mode {
	case "off":
		return "off"
	case "raw":
		if len(body) == 0 {
			return ""
		}
		return string(body)
	default:
		return summarizeJSON(body)
	}
}

func summarizeJSON(body []byte) any {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		if len(trimmed) > 512 {
			return trimmed[:512]
		}
		return trimmed
	}
	return summarizeValue(decoded)
}

func summarizeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for key := range x {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := map[string]any{"keys": keys}
		if msgs, ok := x["messages"].([]any); ok {
			out["messages"] = len(msgs)
		}
		if input, ok := x["input"].([]any); ok {
			out["input"] = len(input)
		}
		if tools, ok := x["tools"].([]any); ok {
			out["tools"] = len(tools)
		}
		if model, ok := x["model"].(string); ok {
			out["model"] = model
		}
		return out
	case []any:
		return map[string]any{"array_len": len(x)}
	default:
		return x
	}
}

// WriteCaptureEvent encodes and appends one MITM capture event.
func WriteCaptureEvent(dir string, event map[string]any, policy CaptureFilePolicy) error {
	raw, err := json.Marshal(event)
	if err != nil {
		slog.Warn("mitm.capture.encode_failed", "capture_dir", dir, "err", err)
		return fmt.Errorf("encode capture event: %w", err)
	}
	return WriteCaptureLine(dir, raw, policy)
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

func ClaudeEnv(ctx context.Context, cfg config.MITMConfig, log *slog.Logger) (map[string]string, error) {
	if !cfg.EnabledDefault || !cfg.EnabledFor("claude") {
		return nil, nil
	}
	proxy, err := EnsureStarted(cfg, log)
	if err != nil {
		return nil, err
	}
	return map[string]string{"ANTHROPIC_BASE_URL": proxy.ClaudeBaseURL()}, nil
}

type CodexOverlay struct {
	Home string
}
