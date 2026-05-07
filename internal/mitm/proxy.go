package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
		log:         log.With("component", "mitm"),
		client:      http.DefaultClient,
		dialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		cfg:         cfg,
		base:        "http://" + ln.Addr().String(),
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

	// Forward upstream response headers, but drop hop-by-hop and
	// length-related headers that the http.Server will recompute.
	// Content-Length is unsafe for streaming responses (SSE), and
	// Go's http.Transport already strips Content-Encoding when it
	// auto-decompresses gzip/deflate, so anything left is honest.
	for key, values := range resp.Header {
		switch strings.ToLower(key) {
		case "content-length", "transfer-encoding", "connection":
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	capture := &limitedBuffer{limit: 16 * 1024}
	flusher, _ := w.(http.Flusher)
	copyErr := streamWithFlush(w, capture, resp.Body, flusher)
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

	upstreamURLForRecord := upstream + r.URL.RequestURI()
	requestEvent := map[string]any{
		"kind":            string(RecordHTTPRequest),
		"t":               currentTime().Unix(),
		"ts":              currentTime().UTC().Format(time.RFC3339Nano),
		"provider":        provider,
		"method":          r.Method,
		"url":             upstreamURLForRecord,
		"path":            r.URL.Path,
		"query":           r.URL.RawQuery,
		"headers":         redactHeaders(r.Header),
		"body_len":        len(body),
		"body":            summarizeBody(cfg.BodyMode, body),
		"request_headers": redactHeaders(r.Header),
		"request_body":    summarizeBody(cfg.BodyMode, body),
	}
	if err := appendCapture(cfg.CaptureDir, requestEvent); err != nil {
		p.log.Warn("mitm.capture.append_failed", "capture_dir", cfg.CaptureDir, "err", err)
	}
	event := map[string]any{
		"kind":             string(RecordHTTPResponse),
		"t":                currentTime().Unix(),
		"ts":               currentTime().UTC().Format(time.RFC3339Nano),
		"provider":         provider,
		"method":           r.Method,
		"url":              upstreamURLForRecord,
		"path":             r.URL.Path,
		"query":            r.URL.RawQuery,
		"status":           resp.StatusCode,
		"duration_ms":      duration.Milliseconds(),
		"headers":          redactHeaders(resp.Header),
		"body_len":         len(captureBody),
		"body":             summarizeBody(cfg.BodyMode, captureBody),
		"request_headers":  redactHeaders(r.Header),
		"response_headers": redactHeaders(resp.Header),
		"request_body":     summarizeBody(cfg.BodyMode, body),
		"response_body":    summarizeBody(cfg.BodyMode, captureBody),
	}
	p.log.Info("mitm.capture.completed",
		"provider", provider,
		"path", r.URL.Path,
		"status", resp.StatusCode,
		"duration_ms", duration.Milliseconds(),
	)
	if err := appendCapture(cfg.CaptureDir, event); err != nil {
		p.log.Warn("mitm.capture.append_failed", "capture_dir", cfg.CaptureDir, "err", err)
	}
	queueBaselineRefresh(cfg, provider, p.log)
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
		switch lower {
		case "authorization",
			"proxy-authorization",
			"cookie",
			"set-cookie",
			"x-api-key",
			"api-key",
			"anthropic-api-key",
			"openai-api-key",
			"x-openai-api-key",
			"x-cursor-token",
			"x-cursor-auth-token":
			out[key] = "<redacted>"
		default:
			out[key] = strings.Join(h.Values(key), ", ")
		}
	}
	return out
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

func appendCapture(dir string, event map[string]any) error {
	dir = expandHome(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "capture.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		return err
	}
	return nil
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
