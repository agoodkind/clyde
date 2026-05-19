// Package anthropic implements Anthropic wire models and helpers.
package anthropic

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/oauth"
)

// sessionID is a per-daemon-process UUIDv4 used for the session
// correlation header. Generated lazily once at the first messages
// request and stable for the lifetime of the daemon.
var sessionID = onceSessionID()

func onceSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure on macOS is effectively impossible.
		// Fall back to a stable string so the header is always set.
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// New builds a Client. If httpClient is nil a 10 minute timeout
// client is used; long timeouts matter because /v1/messages can keep
// a connection open for the full inference window on large outputs.
// cfg carries wire values from [adapter.client_identity] and
// [adapter.oauth]. New does not validate cfg; callers should refuse
// to start when required fields are empty.
func New(httpClient *http.Client, source *oauth.Manager, cfg Config) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{http: httpClient, oauth: managerWrap(source), cfg: cfg}
}

// SystemPromptPrefix returns the configured prefix so callers
// (oauth_handler) can prepend it to outgoing system prompts without
// reaching into the Client struct.
func (c *Client) SystemPromptPrefix() string { return c.cfg.SystemPromptPrefix }

// MessagesURL returns the upstream URL for /v1/messages requests. Used
// by the adapter dispatch layer to populate egress session metadata.
func (c *Client) MessagesURL() string { return c.cfg.MessagesURL }

// UserAgent returns the configured User-Agent so callers can derive
// a semver-like prefix for the billing line without re-parsing config.
func (c *Client) UserAgent() string { return c.cfg.UserAgent }

// CCVersion returns the configured cc_version fallback when User-Agent
// parsing yields no version segment.
func (c *Client) CCVersion() string { return c.cfg.CCVersion }

// CCEntrypoint returns the configured cc_entrypoint suffix for the
// billing line.
func (c *Client) CCEntrypoint() string { return c.cfg.CCEntrypoint }

// managerWrap lets us pass an *oauth.Manager directly while keeping
// the OAuthSource interface for tests.
func managerWrap(m *oauth.Manager) OAuthSource {
	if m == nil {
		return nil
	}
	return OAuthSource(m)
}

// StreamEvents issues a streaming /v1/messages request and invokes sink
// for each decoded stream event (text, tool-use lifecycle, thinking,
// and final stop).
func (c *Client) StreamEvents(ctx context.Context, req Request, sink EventSink) (Usage, string, error) {
	log := anthropicRequestLog.Logger()
	req.Stream = true
	resp, err := c.do(ctx, req)
	if err != nil {
		return Usage{}, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	usage := Usage{}
	stopReason := ""
	blockTypes := make(map[int]string)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)

	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(line[len("data:"):])
			if data == "" {
				continue
			}
			if err := dispatchSSE(currentEvent, data, sink, &usage, &stopReason, blockTypes); err != nil {
				return usage, stopReason, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.WarnContext(ctx, "anthropic.stream.scan_failed",
			"subcomponent", "anthropic",
			"model", req.Model,
			"err", err.Error(),
		)
		return usage, stopReason, fmt.Errorf("anthropic stream scan: %w", err)
	}
	return usage, stopReason, nil
}

// Do executes a native `/v1/messages` request and returns the decoded
// upstream HTTP response for callers that need to preserve Anthropic JSON
// or SSE framing at the adapter boundary.
func (c *Client) Do(ctx context.Context, req Request) (*http.Response, error) {
	return c.do(ctx, req)
}

func (c *Client) do(ctx context.Context, req Request) (*http.Response, error) {
	log := anthropicRequestLog.Logger()
	if c.oauth == nil {
		return nil, errors.New("anthropic client missing oauth source")
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = MaxOutputTokens
	}
	body, err := json.Marshal(req)
	if err != nil {
		log.WarnContext(ctx, "anthropic.messages.marshal_failed",
			"subcomponent", "anthropic",
			"model", req.Model,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}

	token, err := c.oauth.Token(ctx)
	if err != nil {
		log.WarnContext(ctx, "anthropic.messages.auth_lookup_failed",
			"subcomponent", "anthropic",
			"model", req.Model,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("oauth token: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.MessagesURL, bytes.NewReader(body))
	if err != nil {
		log.WarnContext(ctx, "anthropic.messages.request_build_failed",
			"subcomponent", "anthropic",
			"model", req.Model,
			"url", c.cfg.MessagesURL,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("build messages request: %w", err)
	}
	// Wire signals required by the upstream identity check; values come from cfg.

	// Auth + protocol.
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-version", c.cfg.OAuthAnthropicVersion)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if req.Stream {
		// Keep SSE uncompressed. Compressed streaming responses can
		// coalesce small tool argument deltas inside the upstream
		// compressor/decompressor even when our downstream SSE writer
		// flushes every chunk.
		httpReq.Header.Set("Accept-Encoding", "identity")
	} else {
		httpReq.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}

	// CLYDE_PROBE_DROP is a comma-separated list of header names to
	// omit below for debugging. Empty means send the full configured set.
	dropped := probeDropSet()
	setHard := func(key, value string) {
		if _, skip := dropped[strings.ToLower(key)]; skip {
			return
		}
		httpReq.Header.Set(key, value)
	}

	// Wire identity values: prefer the captured flavor, allow
	// cfg.ClientIdentity.* to override (operator escape hatch for
	// testing). req.ExtraBetas merges into the beta header without
	// dropping any flag from the captured set.
	flavor := c.activeFlavor()
	beta := flavor.AnthropicBeta
	if v := strings.TrimSpace(c.cfg.BetaHeader); v != "" {
		beta = v
	}
	if len(req.ExtraBetas) > 0 {
		existing := map[string]struct{}{}
		for f := range strings.SplitSeq(beta, ",") {
			existing[strings.TrimSpace(f)] = struct{}{}
		}
		for _, extra := range req.ExtraBetas {
			extra = strings.TrimSpace(extra)
			if extra == "" {
				continue
			}
			if _, dup := existing[extra]; dup {
				continue
			}
			beta = beta + "," + extra
			existing[extra] = struct{}{}
		}
	}
	setHard("anthropic-beta", beta)

	userAgent := flavor.UserAgent
	if v := strings.TrimSpace(c.cfg.UserAgent); v != "" {
		userAgent = v
	}
	setHard("User-Agent", userAgent)

	// Runtime-derived headers plus defaults.
	for _, h := range freeIdentityHeaders(c) {
		httpReq.Header.Set(h.key, h.value)
	}

	if len(dropped) > 0 {
		keys := make([]string, 0, len(dropped))
		for k := range dropped {
			keys = append(keys, k)
		}
		log.WarnContext(ctx, "anthropic.probe.headers_dropped",
			"subcomponent", "anthropic",
			"dropped", keys,
		)
	}

	log.DebugContext(ctx, "anthropic.messages.request",
		"subcomponent", "anthropic",
		"model", req.Model,
		"url", c.cfg.MessagesURL,
		"body_bytes", len(body),
		"headers", redactedOutboundHeaders(httpReq.Header),
		"body", string(body),
	)

	postStarted := anthropicClock.Now()
	resp, err := c.http.Do(httpReq)
	if resp != nil {
		// We set Accept-Encoding explicitly, so Go's transparent gzip
		// handling is disabled.
		// Swap resp.Body in place with a decoding reader so every
		// downstream consumer (error body readers, SSE stream parser)
		// sees plaintext. Unsupported encodings (br, zstd) leave the
		// body untouched so callers can still inspect bytes for debug.
		decodeResponseBody(resp)
	}
	if err != nil {
		logResponse(slog.LevelError, "anthropic.messages.post_failed", responseEvent{
			Subcomponent: "anthropic",
			Model:        req.Model,
			BodyBytes:    len(body),
			DurationMs:   time.Since(postStarted).Milliseconds(),
			Err:          err.Error(),
		})
		return nil, &UpstreamError{
			Classification: Classify(nil, err),
			Status:         0,
			Message:        fmt.Sprintf("post /v1/messages: %v", err),
			Cause:          err,
			ErrorType:      ErrorKindNone,
		}
	}

	base := responseEvent{
		Subcomponent: "anthropic",
		Model:        req.Model,
		Status:       resp.StatusCode,
		RequestID:    resp.Header.Get("request-id"),
		BodyBytes:    len(body),
		DurationMs:   time.Since(postStarted).Milliseconds(),
		RateLimits:   rateLimitAttrs(resp.Header),
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		errBody := readDecodedBody(resp)
		ev := base
		ev.RetryAfter = resp.Header.Get("retry-after")
		ev.Body = string(errBody)
		ev.BodyBytes = len(errBody)
		logResponse(slog.LevelWarn, "anthropic.ratelimit", ev)

		// Surface unified rate-limit headers to the OnHeaders callback even
		// on 429 so the chat handler can Claim and inject the in-band
		// overage / early-warning notice into the user-facing error. The
		// 200-OK path also calls OnHeaders below; this is the rejection
		// peer of that hook and is intentionally invoked before returning.
		if req.OnHeaders != nil {
			slog.LogAttrs(context.Background(), slog.LevelDebug, "anthropic.notice.headers_observed",
				slog.String("subcomponent", "anthropic"),
				slog.String("phase", "ratelimit_429"),
				slog.String("model", req.Model),
				slog.String("request_id", resp.Header.Get("request-id")),
			)
			req.OnHeaders(resp.Header.Clone())
		}

		// Prefer a friendly message built from the unified rate-limit
		// headers; fall back to the headerless "extra usage required"
		// entitlement message; fall back to the raw body otherwise.
		class := Classify(resp, nil)
		var message string
		switch {
		case FormatRateLimitMessage(resp.Header) != "":
			message = FormatRateLimitMessage(resp.Header)
		case strings.Contains(string(errBody), "Extra usage is required for long context"):
			message = "Extra usage is required for 1M context · enable extra usage at claude.ai/settings/usage, or switch to a standard-context model"
		default:
			message = truncate(string(errBody), 600)
		}
		return nil, &UpstreamError{
			Classification: class,
			Status:         resp.StatusCode,
			Message:        message,
			Cause:          nil,
			ErrorType:      ErrorKindNone,
		}
	}
	if resp.StatusCode != http.StatusOK {
		errBody := readDecodedBody(resp)
		ev := base
		ev.Body = string(errBody)
		ev.BodyBytes = len(errBody)
		logResponse(slog.LevelError, "anthropic.messages.upstream_error", ev)
		return nil, &UpstreamError{
			Classification: Classify(resp, nil),
			Status:         resp.StatusCode,
			Message:        truncate(string(errBody), 600),
			Cause:          nil,
			ErrorType:      ErrorKindNone,
		}
	}
	logResponse(slog.LevelInfo, "anthropic.messages.connected", base)
	if req.OnHeaders != nil {
		req.OnHeaders(resp.Header.Clone())
	}
	c.maybeAttachWireCapture(ctx, resp, base)
	return resp, nil
}

// maybeAttachWireCapture is the do() boundary helper that filters Off and
// delegates to the per-mode emitter; keeps do()'s cognitive complexity
// budget under the lint threshold.
func (c *Client) maybeAttachWireCapture(ctx context.Context, resp *http.Response, base responseEvent) {
	mode := c.cfg.WireCaptureMode
	if mode == "" || mode == WireCaptureOff {
		return
	}
	attachWireCapture(ctx, mode, resp, base)
}

// wireCaptureBodyCap bounds how many bytes a Full-mode capture buffers in
// memory per response. SSE responses can exceed this on long thinking turns;
// we truncate and surface the truncation flag so the operator knows to flip
// rotation up if they need full bodies.
const wireCaptureBodyCap = 2 * 1024 * 1024

// attachWireCapture emits the per-success-path wire-capture event and, for
// Full mode, swaps resp.Body for a tee reader so the streamed SSE body is
// buffered (capped) and logged on Close. Summary mode emits a fingerprint
// immediately and leaves resp.Body untouched. Off is filtered by the caller.
func attachWireCapture(ctx context.Context, mode WireCaptureMode, resp *http.Response, base responseEvent) {
	headers := redactedOutboundHeaders(resp.Header)
	switch mode {
	case WireCaptureOff:
		// Caller filters Off before reaching this site; explicit case
		// keeps the switch exhaustive over the closed enum.
		return
	case WireCaptureSummaryOnly:
		emitWireCaptureSummary(ctx, mode, base, headers)
	case WireCaptureFull:
		// Detach from request cancellation so the on-close emission still
		// fires after the SSE stream completes; preserve correlation
		// values via context.WithoutCancel.
		emitCtx := context.WithoutCancel(ctx)
		resp.Body = newCaptureTee(resp.Body, wireCaptureBodyCap, func(captured []byte, truncated bool, totalRead int) {
			emitWireCaptureFull(emitCtx, mode, base, headers, captured, truncated, totalRead)
		})
	}
}

func emitWireCaptureSummary(ctx context.Context, mode WireCaptureMode, base responseEvent, headers map[string]string) {
	anthropicWireCaptureLog.Logger().LogAttrs(ctx, slog.LevelInfo, "adapter.providers.anthropic.wire_capture",
		slog.String("subcomponent", "anthropic"),
		slog.String("mode", string(mode)),
		slog.String("model", base.Model),
		slog.Int("status", base.Status),
		slog.String("request_id", base.RequestID),
		slog.Int("body_bytes_request", base.BodyBytes),
		slog.Int64("duration_ms", base.DurationMs),
		slog.Any("response_headers", headers),
	)
}

func emitWireCaptureFull(ctx context.Context, mode WireCaptureMode, base responseEvent, headers map[string]string, captured []byte, truncated bool, totalRead int) {
	anthropicWireCaptureLog.Logger().LogAttrs(ctx, slog.LevelInfo, "adapter.providers.anthropic.wire_capture",
		slog.String("subcomponent", "anthropic"),
		slog.String("mode", string(mode)),
		slog.String("model", base.Model),
		slog.Int("status", base.Status),
		slog.String("request_id", base.RequestID),
		slog.Int("body_bytes_request", base.BodyBytes),
		slog.Int("body_bytes_response", totalRead),
		slog.Int("body_bytes_captured", len(captured)),
		slog.Bool("truncated", truncated),
		slog.Int64("duration_ms", base.DurationMs),
		slog.Any("response_headers", headers),
		slog.String("body", string(captured)),
	)
}

// captureTee is the [io.ReadCloser] shim Full-mode wire capture wraps around
// resp.Body. Reads pass through unchanged; bytes also accumulate in an
// internal buffer up to cap. On Close, onClose fires once with the captured
// slice, the truncation flag, and the total read count. Stream parsers
// downstream see a normal ReadCloser, so the lifecycle is invisible.
type captureTee struct {
	inner     io.ReadCloser
	buf       bytes.Buffer
	cap       int
	totalRead int
	truncated bool
	onClose   func(captured []byte, truncated bool, totalRead int)
	closed    bool
}

func newCaptureTee(inner io.ReadCloser, capBytes int, onClose func(captured []byte, truncated bool, totalRead int)) *captureTee {
	return &captureTee{
		inner:     inner,
		buf:       bytes.Buffer{},
		cap:       capBytes,
		totalRead: 0,
		truncated: false,
		onClose:   onClose,
		closed:    false,
	}
}

// Read passes through the inner Read; bytes also accumulate (capped) in the
// internal buffer. Errors are wrapped with %w so [errors.Is] callers still
// detect [io.EOF]. Non-EOF errors also emit on the wire-capture concern.
func (t *captureTee) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	t.recordCapturedBytes(p, n)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, io.EOF) {
		slog.Warn("anthropic.wire_capture.tee_read_failed",
			"subcomponent", "anthropic",
			"err", err.Error(),
		)
	}
	return n, fmt.Errorf("captureTee read: %w", err)
}

// Close closes the inner body and fires onClose exactly once with the
// captured slice, truncation flag, and total read count. Inner Close errors
// are wrapped with context for callers; the on-close emission still fires
// regardless so the captured body is always logged.
func (t *captureTee) Close() error {
	err := t.inner.Close()
	if !t.closed {
		t.closed = true
		if t.onClose != nil {
			t.onClose(t.buf.Bytes(), t.truncated, t.totalRead)
		}
	}
	if err == nil {
		return nil
	}
	slog.Warn("anthropic.wire_capture.tee_close_failed",
		"subcomponent", "anthropic",
		"err", err.Error(),
	)
	return fmt.Errorf("captureTee close: %w", err)
}

func (t *captureTee) recordCapturedBytes(p []byte, n int) {
	if n <= 0 {
		return
	}
	t.totalRead += n
	if t.buf.Len() >= t.cap {
		t.truncated = true
		return
	}
	remain := t.cap - t.buf.Len()
	if n <= remain {
		t.buf.Write(p[:n])
		return
	}
	t.buf.Write(p[:remain])
	t.truncated = true
}

// probeDropSet returns the lowercased set of header names in CLYDE_PROBE_DROP.
func probeDropSet() map[string]struct{} {
	raw := strings.TrimSpace(os.Getenv("CLYDE_PROBE_DROP"))
	if raw == "" {
		return nil
	}
	out := map[string]struct{}{}
	for part := range strings.SplitSeq(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name != "" {
			out[name] = struct{}{}
		}
	}
	return out
}

type freeHeader struct {
	key   string
	value string
}

// activeFlavor returns the captured wire flavor we mirror on outbound
// requests. Today this is the claude-cli interactive flavor; future
// work can pick a flavor per caller shape (e.g. a probe flavor for
// short non-streaming requests). The flavor is generated from the local XDG
// MITM baseline so identity drift surfaces in daemon-owned drift checks.
func (c *Client) activeFlavor() WireFlavor {
	return WireFlavorClaudeCodeInteractive
}

// freeIdentityHeaders returns the per-request identity headers that
// are not auth or cache-control. Static values come from the captured
// WireFlavor; runtime-only fields (per-process session id, timeout
// derived from c.http.Timeout) override the captured value.
//
// cfg.ClientIdentity.* still wins when explicitly set so operators
// can override for testing without regenerating wire_flavors_gen.go.
func freeIdentityHeaders(c *Client) []freeHeader {
	flavor := c.activeFlavor()
	out := make([]freeHeader, 0, len(flavor.StaticHeaders)+1)
	for _, h := range flavor.StaticHeaders {
		val := h.Value
		switch strings.ToLower(h.Name) {
		case "x-stainless-package-version":
			if v := strings.TrimSpace(c.cfg.StainlessPackageVersion); v != "" {
				val = v
			}
		case "x-stainless-runtime":
			if v := strings.TrimSpace(c.cfg.StainlessRuntime); v != "" {
				val = v
			}
		case "x-stainless-runtime-version":
			if v := strings.TrimSpace(c.cfg.StainlessRuntimeVersion); v != "" {
				val = v
			}
		case "x-stainless-timeout":
			val = c.stainlessTimeout()
		}
		out = append(out, freeHeader{key: h.Name, value: val})
	}
	// Runtime-only header not present in the captured flavor (per-process UUID).
	out = append(out, freeHeader{key: "X-Claude-Code-Session-Id", value: sessionID})
	return out
}

func (c *Client) stainlessTimeout() string {
	if c.http.Timeout == 0 {
		return "600"
	}
	return strconv.Itoa(int(c.http.Timeout / time.Second))
}

// redactedOutboundHeaders returns a flat map[string]string of the
// headers we set on the outbound messages request, with secret
// values masked. Keys are lowercased so log diffs are deterministic
// and friendly to grep. Used by the anthropic.messages.request slog
// event so debug captures show exactly what we sent.
// decodeResponseBody swaps resp.Body for a decoding reader matching
// resp.Header.Get("Content-Encoding"). Stdlib covers gzip and deflate;
// br and zstd are passed through untouched (the upstream rarely picks
// them when gzip is also offered). Removes the Content-Encoding
// header on success so callers don't double-decode.
func decodeResponseBody(resp *http.Response) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return
	}
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			anthropicRequestLog.Logger().Warn("anthropic.response.gzip_decode_failed",
				"subcomponent", "anthropic", "err", err.Error())
			return
		}
		resp.Body = &decodedBody{r: zr, src: resp.Body}
		resp.Header.Del("Content-Encoding")
	case "deflate":
		fr := flate.NewReader(resp.Body)
		resp.Body = &decodedBody{r: fr, src: resp.Body}
		resp.Header.Del("Content-Encoding")
	default:
		// br, zstd, etc. Keep raw bytes; callers will see binary in
		// the slog body field if the server actually picks one of these.
		anthropicRequestLog.Logger().Warn("anthropic.response.unsupported_encoding",
			"subcomponent", "anthropic", "encoding", enc)
	}
}

// decodedBody wraps a decompressing reader so Close() also closes the
// underlying response body the http client owns.
type decodedBody struct {
	r   io.ReadCloser
	src io.ReadCloser
}

func (d *decodedBody) Read(p []byte) (int, error) { return d.r.Read(p) }
func (d *decodedBody) Close() error {
	rerr := d.r.Close()
	serr := d.src.Close()
	if rerr != nil {
		return rerr
	}
	return serr
}

// readDecodedBody reads resp.Body to EOF and closes it. Assumes
// decodeResponseBody already wrapped Body if Content-Encoding was set.
// Returns the bytes the caller would have seen as if no encoding was
// applied.
func readDecodedBody(resp *http.Response) []byte {
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func redactedOutboundHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for key, values := range h {
		lk := strings.ToLower(key)
		joined := strings.Join(values, ", ")
		switch lk {
		case "authorization":
			out[lk] = fmt.Sprintf("Bearer <redacted len=%d>", len(joined)-len("Bearer "))
		case "x-api-key", "cookie", "proxy-authorization":
			out[lk] = "<redacted>"
		default:
			out[lk] = joined
		}
	}
	return out
}
