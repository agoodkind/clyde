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

	"goodkind.io/clyde/internal/clock"
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
// [adapter.anthropic.oauth]. source supplies the bearer token per request. A
// nil source is tolerated at construction but every request then fails with a
// missing oauth source error. New does not validate cfg; callers should refuse
// to start when required fields are empty.
func New(httpClient *http.Client, source OAuthSource, cfg Config) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{http: httpClient, oauth: source, cfg: cfg, flavorLoader: newWireFlavorsLoader()}
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

	usage := Usage{InputTokens: 0, OutputTokens: 0, CacheCreationInputTokens: 0, CacheReadInputTokens: 0}
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
		log.WarnContext(ctx, "anthropic.stream.scan_failed", "concern", "adapter.providers.anthropic.sse", "subcomponent", "anthropic",
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

	flavor, body, err := c.prepareRequestBody(ctx, req)
	if err != nil {
		return nil, err
	}

	token, err := c.oauth.Token(ctx)
	if err != nil {
		log.WarnContext(ctx, "anthropic.messages.auth_lookup_failed", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("oauth token: %w", err)
	}

	httpReq, err := c.buildMessagesRequest(ctx, req, body, token, flavor)
	if err != nil {
		return nil, err
	}

	postStarted := clock.Now()
	ex := egressExchange{
		method:     httpReq.Method,
		host:       httpReq.URL.Host,
		path:       httpReq.URL.Path,
		reqType:    httpReq.Header.Get("Content-Type"),
		reqHeaders: httpReq.Header,
		reqBody:    body,
		started:    postStarted,
	}
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
		logResponse(ctx, slog.LevelError, "anthropic.messages.post_failed", responseEvent{
			Subcomponent: "anthropic",
			Model:        req.Model,
			Status:       0,
			RequestID:    "",
			BodyBytes:    len(body),
			DurationMs:   clock.Since(postStarted).Milliseconds(),
			RateLimits:   nil,
			RetryAfter:   "",
			Body:         "",
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

	resp, postStarted = c.maybeRetryOn401(ctx, req, body, token, flavor, resp, postStarted)

	base := responseEvent{
		Subcomponent: "anthropic",
		Model:        req.Model,
		Status:       resp.StatusCode,
		RequestID:    resp.Header.Get("Request-Id"),
		BodyBytes:    len(body),
		DurationMs:   clock.Since(postStarted).Milliseconds(),
		RateLimits:   rateLimitAttrs(resp.Header), RetryAfter: "", Body: "", Err: "",
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, c.handle429Response(ctx, req, resp, base, ex)
	}
	if resp.StatusCode != http.StatusOK {
		errBody := readDecodedBody(resp)
		ev := base
		ev.Body = string(errBody)
		ev.BodyBytes = len(errBody)
		logResponse(ctx, slog.LevelError, "anthropic.messages.upstream_error", ev)
		c.recordEgress(ctx, ex, resp.StatusCode, resp.Header, errBody)
		return nil, &UpstreamError{
			Classification: Classify(resp, nil),
			Status:         resp.StatusCode,
			Message:        truncate(string(errBody), 600),
			Cause:          nil,
			ErrorType:      ErrorKindNone,
		}
	}
	logResponse(ctx, slog.LevelInfo, "anthropic.messages.connected", base)
	if req.OnHeaders != nil {
		req.OnHeaders(resp.Header.Clone())
	}
	c.attachEgressObservers(ctx, resp, base, ex)
	return resp, nil
}

// handle429Response builds the typed UpstreamError for a 429 reply, logs the
// rate-limit event, and fires the OnHeaders callback. The returned error is
// the value do() propagates to its caller.
func (c *Client) handle429Response(ctx context.Context, req Request, resp *http.Response, base responseEvent, ex egressExchange) error {
	errBody := readDecodedBody(resp)
	ev := base
	ev.RetryAfter = resp.Header.Get("Retry-After")
	ev.Body = string(errBody)
	ev.BodyBytes = len(errBody)
	logResponse(ctx, slog.LevelWarn, "anthropic.ratelimit", ev)
	c.recordEgress(ctx, ex, resp.StatusCode, resp.Header, errBody)

	// Surface unified rate-limit headers to the OnHeaders callback even
	// on 429 so the chat handler can Claim and inject the in-band
	// overage / early-warning notice into the user-facing error. The
	// 200-OK path also calls OnHeaders; this is the rejection peer of
	// that hook and is intentionally invoked before returning.
	if req.OnHeaders != nil {
		slog.LogAttrs(ctx, slog.LevelDebug, "anthropic.notice.headers_observed", slog.String("concern", "adapter.notice"), slog.String("subcomponent", "anthropic"),
			slog.String("phase", "ratelimit_429"),
			slog.String("model", req.Model),
			slog.String("request_id", resp.Header.Get("Request-Id")),
		)
		req.OnHeaders(resp.Header.Clone())
	}

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
	return &UpstreamError{
		Classification: class,
		Status:         resp.StatusCode,
		Message:        message,
		Cause:          nil,
		ErrorType:      ErrorKindNone,
	}
}

// prepareRequestBody resolves the wire flavor and produces the marshaled
// outbound body the learned baseline shapes. It resolves the flavor
// before marshaling so the captured billing attestation can be
// substituted into the attribution block, then validates the body's
// top-level field set against the learned required fields. A missing or
// invalid baseline returns the typed sentinel unchanged so the dispatch
// layer can map it to an HTTP 503 rather than sending a wrong-shaped
// request with no identity.
func (c *Client) prepareRequestBody(ctx context.Context, req Request) (WireFlavor, []byte, error) {
	log := anthropicRequestLog.Logger()
	flavor, err := c.activeFlavor(featureVectorForRequest(req))
	if err != nil {
		log.WarnContext(ctx, "anthropic.messages.wire_baseline_unavailable", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"baseline_path", c.cfg.WireBaselinePath,
			"err", err.Error(),
		)
		return WireFlavor{}, nil, err
	}
	applyBillingAttestation(req.SystemBlocks, flavor.BillingAttestation)

	body, err := json.Marshal(req)
	if err != nil {
		log.WarnContext(ctx, "anthropic.messages.marshal_failed", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"err", err.Error(),
		)
		return WireFlavor{}, nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	checkBodyShape(ctx, req.Model, body, flavor)
	return flavor, body, nil
}

// buildMessagesRequest assembles the outbound /v1/messages POST: it
// constructs the [http.Request] with the marshaled body bytes, attaches the
// bearer token, and applies every wire identity header the captured flavor
// and config dictate. The helper exists so the on-401 retry path can rebuild
// the request with a fresh token without re-marshaling the body. The flavor
// is resolved once by the caller and threaded through so the header build
// and the body shaping in do() agree on a single resolved baseline.
func (c *Client) buildMessagesRequest(ctx context.Context, req Request, body []byte, token string, flavor WireFlavor) (*http.Request, error) {
	log := anthropicRequestLog.Logger()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.MessagesURL, bytes.NewReader(body))
	if err != nil {
		log.WarnContext(ctx, "anthropic.messages.request_build_failed", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"url", c.cfg.MessagesURL,
			"err", err.Error(),
		)
		return nil, fmt.Errorf("build messages request: %w", err)
	}
	dropped := c.applyMessagesHeaders(httpReq, req, token, flavor)
	if len(dropped) > 0 {
		keys := make([]string, 0, len(dropped))
		for k := range dropped {
			keys = append(keys, k)
		}
		log.WarnContext(ctx, "anthropic.probe.headers_dropped", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"dropped", keys,
		)
	}
	log.DebugContext(ctx, "anthropic.messages.request", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
		"model", req.Model,
		"url", c.cfg.MessagesURL,
		"body_bytes", len(body),
		"headers", redactedOutboundHeaders(httpReq.Header),
		"body", string(body),
	)
	return httpReq, nil
}

// applyMessagesHeaders sets every outbound header /v1/messages requires onto
// httpReq: bearer auth, protocol version, content negotiation, the captured
// wire flavor identity headers, and the optional CLYDE_PROBE_DROP exclusion
// set. The returned drop set is non-nil only when an operator opted into
// header omission for local debugging. The flavor is resolved by do() and
// passed in, so a missing or invalid baseline is rejected before this point
// and the request never goes out with no identity.
func (c *Client) applyMessagesHeaders(httpReq *http.Request, req Request, token string, flavor WireFlavor) map[string]struct{} {
	httpReq.Header.Set("Authorization", "Bearer "+token)
	// Prefer the learned baseline's anthropic-version; the loader
	// validates it is non-empty so the flavor wins in practice. Fall
	// back to the configured value only when the flavor carries none.
	anthropicVersion := strings.TrimSpace(flavor.AnthropicVersion)
	if anthropicVersion == "" {
		anthropicVersion = c.cfg.OAuthAnthropicVersion
	}
	httpReq.Header.Set("Anthropic-Version", anthropicVersion)
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

	dropped := probeDropSet()
	setHard := func(key, value string) {
		if _, skip := dropped[strings.ToLower(key)]; skip {
			return
		}
		httpReq.Header.Set(key, value)
	}

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
	if len(c.cfg.BetaSuppress) > 0 {
		filtered, removed := suppressBetaFlags(beta, c.cfg.BetaSuppress)
		if len(removed) > 0 {
			anthropicRequestLog.Logger().DebugContext(httpReq.Context(), "anthropic.messages.beta_suppressed",
				"concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
				"model", req.Model,
				"removed", removed,
			)
			beta = filtered
		}
	}
	setHard("Anthropic-Beta", beta)

	userAgent := flavor.UserAgent
	if v := strings.TrimSpace(c.cfg.UserAgent); v != "" {
		userAgent = v
	}
	setHard("User-Agent", userAgent)

	for _, h := range freeIdentityHeaders(c, flavor) {
		httpReq.Header.Set(h.key, h.value)
	}
	return dropped
}

// suppressBetaFlags removes every flag in suppress from the comma-joined
// anthropic-beta value, preserving the order of the surviving flags.
// Matching is exact per trimmed token and case-insensitive. It returns
// the rebuilt header value and the list of removed flags (in the order
// they appeared in beta) so the caller can log exactly what changed. A
// flag in suppress that is absent from beta is silently ignored.
func suppressBetaFlags(beta string, suppress []string) (string, []string) {
	if strings.TrimSpace(beta) == "" || len(suppress) == 0 {
		return beta, nil
	}
	drop := make(map[string]struct{}, len(suppress))
	for _, s := range suppress {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		drop[strings.ToLower(s)] = struct{}{}
	}
	if len(drop) == 0 {
		return beta, nil
	}
	kept := make([]string, 0)
	removed := make([]string, 0)
	for f := range strings.SplitSeq(beta, ",") {
		flag := strings.TrimSpace(f)
		if flag == "" {
			continue
		}
		if _, skip := drop[strings.ToLower(flag)]; skip {
			removed = append(removed, flag)
			continue
		}
		kept = append(kept, flag)
	}
	if len(removed) == 0 {
		return beta, nil
	}
	return strings.Join(kept, ","), removed
}

// maybeRetryOn401 is the do() boundary helper that invokes one on-401 retry
// when the initial response is a 401, and swaps the response and post-start
// timestamp so downstream logging measures the retry duration. When the
// initial response is not 401 or the retry is skipped or fails, the original
// response and timestamp pass through unchanged.
func (c *Client) maybeRetryOn401(ctx context.Context, req Request, body []byte, token string, flavor WireFlavor, resp *http.Response, postStarted time.Time) (*http.Response, time.Time) {
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, postStarted
	}
	retried, retryDuration, retryErr := c.retryAfter401(ctx, req, body, token, flavor, resp)
	if retryErr != nil || retried == nil {
		return resp, postStarted
	}
	return retried, clock.Now().Add(-retryDuration)
}

// retryAfter401 runs one recovery attempt for an upstream 401. It drains and
// closes the original response body, asks the OAuth source for a recovered
// token, and posts the same request body once more with a freshly built
// [http.Request]. A nil return value means the retry was skipped (oauth
// reported the token is unchanged) or failed; the caller falls through to the
// existing classifier with the original 401 in that case.
func (c *Client) retryAfter401(ctx context.Context, req Request, body []byte, failedToken string, flavor WireFlavor, originalResp *http.Response) (*http.Response, time.Duration, error) {
	log := anthropicRequestLog.Logger()
	log.InfoContext(ctx, "anthropic.messages.auth_retry_attempted", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
		"model", req.Model,
		"attempt", 1,
	)
	// Read and close the original 401 body, then replace it with a reader
	// over the saved bytes. The retry path consumes the underlying
	// connection either way; the in-memory copy lets the fall-through
	// classifier still emit the upstream error text in error.message when
	// the retry is skipped (oauth reported the token is unchanged) or fails.
	savedBody, _ := io.ReadAll(originalResp.Body)
	_ = originalResp.Body.Close()
	originalResp.Body = io.NopCloser(bytes.NewReader(savedBody))

	refresher, ok := c.oauth.(OAuthRefresher)
	if !ok {
		log.WarnContext(ctx, "anthropic.messages.auth_retry_skipped", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"reason", "auth_refresh_unavailable",
		)
		return nil, 0, errors.New("auth refresh unavailable")
	}
	freshToken, recoverErr := refresher.ForceRefresh(ctx)
	if recoverErr != nil {
		log.WarnContext(ctx, "anthropic.messages.auth_retry_skipped", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"err", recoverErr.Error(),
		)
		return nil, 0, fmt.Errorf("anthropic auth-retry recover: %w", recoverErr)
	}
	if freshToken == "" || freshToken == failedToken {
		log.WarnContext(ctx, "anthropic.messages.auth_retry_skipped", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"reason", "token_unchanged",
		)
		return nil, 0, errors.New("token unchanged after recovery")
	}

	retryReq, buildErr := c.buildMessagesRequest(ctx, req, body, freshToken, flavor)
	if buildErr != nil {
		return nil, 0, buildErr
	}
	retryStarted := clock.Now()
	retryResp, retryErr := c.http.Do(retryReq)
	if retryResp != nil {
		decodeResponseBody(retryResp)
	}
	if retryErr != nil {
		log.WarnContext(ctx, "anthropic.messages.auth_retry_post_failed", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"err", retryErr.Error(),
		)
		return nil, 0, fmt.Errorf("anthropic auth-retry post: %w", retryErr)
	}
	if retryResp.StatusCode != http.StatusUnauthorized {
		log.InfoContext(ctx, "anthropic.messages.auth_retry_recovered", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic",
			"model", req.Model,
			"attempt", 1,
			"status", retryResp.StatusCode,
			"duration_ms", clock.Since(retryStarted).Milliseconds(),
		)
	}
	return retryResp, clock.Since(retryStarted), nil
}

// wireCaptureBodyCap bounds how many bytes a Full-mode wire-capture log buffers
// in memory per response. SSE responses can exceed this on long thinking turns;
// we truncate and surface the truncation flag so the operator knows to flip
// rotation up if they need full bodies.
const wireCaptureBodyCap = 2 * 1024 * 1024

func emitWireCaptureSummary(ctx context.Context, mode WireCaptureMode, base responseEvent, headers map[string]string) {
	anthropicWireCaptureLog.Logger().LogAttrs(ctx, slog.LevelInfo, "adapter.providers.anthropic.wire_capture", slog.String("concern", "adapter.providers.anthropic.wire_capture"), slog.String("subcomponent", "anthropic"),
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
	anthropicWireCaptureLog.Logger().LogAttrs(ctx, slog.LevelInfo, "adapter.providers.anthropic.wire_capture", slog.String("concern", "adapter.providers.anthropic.wire_capture"), slog.String("subcomponent", "anthropic"),
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
// internal buffer. A terminal [io.EOF] is returned bare, not wrapped, because
// the SSE consumer is a [bufio.Scanner] whose Err() compares the read error
// against [io.EOF] with == (see stdlib bufio/scan.go), so a wrapped EOF would
// be mistaken for a real failure and abort the stream scan. Genuine non-EOF
// errors are wrapped with context and emitted on the wire-capture concern.
func (t *captureTee) Read(p []byte) (int, error) {
	n, err := t.inner.Read(p)
	t.recordCapturedBytes(p, n)
	if err == nil {
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	slog.Warn("anthropic.wire_capture.tee_read_failed", "concern", "adapter.providers.anthropic.wire_capture", "subcomponent", "anthropic",
		"err", err.Error(),
	)
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
	slog.Warn("anthropic.wire_capture.tee_close_failed", "concern", "adapter.providers.anthropic.wire_capture", "subcomponent", "anthropic",
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
// requests. The learned claude-cli interactive flavor is selected by
// the request feature vector captured in the baseline. The flavor is
// loaded at request time from the daemon-owned MITM baseline so
// identity drift surfaces in daemon-owned drift checks. There is no
// compiled-in fallback: a missing, invalid, or unseeded baseline error
// returns a typed sentinel-bearing error so the caller can map it to an
// HTTP 503.
func (c *Client) activeFlavor(featureVector WireFlavorFeatureVector) (WireFlavor, error) {
	var zero WireFlavor
	if c.flavorLoader == nil {
		c.flavorLoader = newWireFlavorsLoader()
	}
	flavors, err := c.flavorLoader.Load(c.cfg.WireBaselinePath)
	if err != nil {
		// The loader logs the specific cause on its concern; return the
		// typed sentinel-bearing error unchanged so the dispatch layer
		// can map it to an operator-actionable HTTP 503.
		return zero, err
	}
	flavor, err := selectInteractiveFlavor(flavors, featureVector)
	if err != nil {
		if errors.Is(err, ErrBaselineInvalid) {
			slog.Warn("anthropic.wire_baseline.no_interactive_flavor", "concern", wireBaselineConcern, "subcomponent", "anthropic",
				"baseline_path", c.cfg.WireBaselinePath,
				"model", featureVector.ModelID,
			)
		}
		if errors.Is(err, ErrFlavorUnseeded) {
			slog.Warn("anthropic.wire_baseline.flavor_unseeded", "concern", wireBaselineConcern, "subcomponent", "anthropic",
				"baseline_path", c.cfg.WireBaselinePath,
				"model", featureVector.ModelID,
				"context_1m", featureVector.Context1M,
				"thinking_mode", string(featureVector.ThinkingMode),
				"structured_output_present", featureVector.StructuredOutputPresent,
				"tools_present", featureVector.ToolsPresent,
			)
		}
		return zero, err
	}
	return flavor, nil
}

// freeIdentityHeaders returns the per-request identity headers that
// are not auth or cache-control. Static values come from the resolved
// WireFlavor; runtime-only fields (per-process session id, timeout
// derived from c.http.Timeout) override the captured value.
//
// cfg.ClientIdentity.* still wins when explicitly set so operators can
// override for testing without re-seeding the MITM baseline.
func freeIdentityHeaders(c *Client, flavor WireFlavor) []freeHeader {
	out := make([]freeHeader, 0, len(flavor.StaticHeaders)+1)
	for _, h := range flavor.StaticHeaders {
		val := h.Value
		switch stainlessHeaderName(strings.ToLower(h.Name)) {
		case stainlessHeaderPackageVersion:
			if v := strings.TrimSpace(c.cfg.StainlessPackageVersion); v != "" {
				val = v
			}
		case stainlessHeaderRuntime:
			if v := strings.TrimSpace(c.cfg.StainlessRuntime); v != "" {
				val = v
			}
		case stainlessHeaderRuntimeVersion:
			if v := strings.TrimSpace(c.cfg.StainlessRuntimeVersion); v != "" {
				val = v
			}
		case stainlessHeaderTimeout:
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
// stainlessHeaderName enumerates the lowercase header names the
// Anthropic client tunes per the configured Stainless identity.
type stainlessHeaderName string

const (
	stainlessHeaderPackageVersion stainlessHeaderName = "x-stainless-package-version"
	stainlessHeaderRuntime        stainlessHeaderName = "x-stainless-runtime"
	stainlessHeaderRuntimeVersion stainlessHeaderName = "x-stainless-runtime-version"
	stainlessHeaderTimeout        stainlessHeaderName = "x-stainless-timeout"
)

// httpContentEncoding enumerates the Content-Encoding values the
// Anthropic client decodes inline before returning the response body
// to higher layers.
type httpContentEncoding string

const (
	httpContentEncodingGzip    httpContentEncoding = "gzip"
	httpContentEncodingDeflate httpContentEncoding = "deflate"
)

// redactedHeaderName enumerates the lowercase header names that get
// scrubbed in [redactedOutboundHeaders] before they reach the log
// sink.
type redactedHeaderName string

const (
	redactedHeaderAuthorization      redactedHeaderName = "authorization"
	redactedHeaderXAPIKey            redactedHeaderName = "x-api-key"
	redactedHeaderCookie             redactedHeaderName = "cookie"
	redactedHeaderProxyAuthorization redactedHeaderName = "proxy-authorization"
)

func decodeResponseBody(resp *http.Response) {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return
	}
	switch httpContentEncoding(enc) {
	case httpContentEncodingGzip:
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			anthropicRequestLog.Logger().Warn("anthropic.response.gzip_decode_failed", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic", "err", err.Error())
			return
		}
		resp.Body = &decodedBody{r: zr, src: resp.Body}
		resp.Header.Del("Content-Encoding")
	case httpContentEncodingDeflate:
		fr := flate.NewReader(resp.Body)
		resp.Body = &decodedBody{r: fr, src: resp.Body}
		resp.Header.Del("Content-Encoding")
	default:
		// br, zstd, etc. Keep raw bytes; callers will see binary in
		// the slog body field if the server actually picks one of these.
		anthropicRequestLog.Logger().Warn("anthropic.response.unsupported_encoding", "concern", "adapter.providers.anthropic.request", "subcomponent", "anthropic", "encoding", enc)
	}
}

// decodedBody wraps a decompressing reader so Close() also closes the
// underlying response body the http client owns.
type decodedBody struct {
	r   io.ReadCloser
	src io.ReadCloser
}

func (d *decodedBody) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	switch {
	case err == nil:
		return n, nil
	case errors.Is(err, io.EOF):
		return n, io.EOF
	default:
		return n, fmt.Errorf("read decoded anthropic body: %w", err)
	}
}

func (d *decodedBody) Close() error {
	rerr := d.r.Close()
	serr := d.src.Close()
	if rerr != nil {
		slog.Warn("adapter.anthropic.decoded_body.reader_close_failed", "concern", "adapter.providers.anthropic.request", "err", rerr)
		return fmt.Errorf("close decoded anthropic reader: %w", rerr)
	}
	if serr != nil {
		slog.Warn("adapter.anthropic.decoded_body.source_close_failed", "concern", "adapter.providers.anthropic.request", "err", serr)
		return fmt.Errorf("close anthropic source body: %w", serr)
	}
	return nil
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
		switch redactedHeaderName(lk) {
		case redactedHeaderAuthorization:
			out[lk] = fmt.Sprintf("Bearer <redacted len=%d>", len(joined)-len("Bearer "))
		case redactedHeaderXAPIKey, redactedHeaderCookie, redactedHeaderProxyAuthorization:
			out[lk] = "<redacted>"
		default:
			out[lk] = joined
		}
	}
	return out
}
