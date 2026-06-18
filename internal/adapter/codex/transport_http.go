package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterretry "goodkind.io/clyde/internal/adapter/retry"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

// HTTPTransportConfig carries everything runHTTPTransportEvents needs to
// POST one Codex Responses request over plain HTTP and stream the SSE
// reply. It is the HTTP-transport sibling of WebsocketTransportConfig.
// RunDirect populates it when the websocket upgrade asks for the HTTP
// fallback (HTTP 426) or when the websocket transport is disabled.
type HTTPTransportConfig struct {
	URL                string
	HTTPClient         *http.Client
	Token              string
	RequestID          string
	CursorRequestID    string
	Correlation        correlation.Context
	Alias              string
	ConversationID     string
	InstallationID     string
	WindowID           string
	TurnMetadata       string
	Log                *slog.Logger
	RoundTripEncrypted RoundTripEncrypted
	RetryPolicies      []adapterretry.Policy
	BeforeAttempt      func(ctx context.Context, attemptNo int) (context.Context, func(string))
	// AuthRefresh, when non-nil, is called when the HTTP request
	// returns 401 or 403. It returns a refreshed access token (or an
	// error if the refresh itself failed) so the transport can retry
	// once with the new token before propagating the failure.
	AuthRefresh func(ctx context.Context) (string, error)
	// WireIdentity carries the baseline-driven outbound wire identity
	// (originator, user-agent, beta-features, attestation) projected
	// from the daemon-owned MITM codex-cli baseline. The openai-beta
	// upgrade header is stripped on the HTTP transport, so its value is
	// unused here. Empty fields fall back to the compiled-in identity
	// constants.
	WireIdentity WireIdentity
	// CaptureStore, when non-nil, receives one capture.Record for the
	// exchange tagged client="adapter.codex". Nil records nothing.
	CaptureStore *capture.Store
	// StripWireFlags lists capability tokens to drop from the outbound
	// codex capability headers, from the provider-neutral
	// [adapter].strip_wire_flags config. Empty replays the learned headers
	// untouched.
	StripWireFlags []string
}

const httpErrorBodySnippetLimit = 512

// buildResponsesHTTPHeaders builds the header set for a Codex Responses
// HTTP request. It reuses BuildResponsesWebsocketHeaders so the shared
// Codex headers stay defined in exactly one place, then strips the
// websocket-only OpenAI-Beta upgrade header and adds the HTTP content
// negotiation headers (Content-Type and the text/event-stream Accept).
func buildResponsesHTTPHeaders(cfg HTTPTransportConfig) http.Header {
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:            cfg.RequestID,
		ConversationID:       cfg.ConversationID,
		Correlation:          cfg.Correlation,
		Token:                cfg.Token,
		InstallationID:       cfg.InstallationID,
		WindowID:             cfg.WindowID,
		BetaFeatures:         cfg.WireIdentity.BetaFeatures,
		TurnState:            NewTurnState(),
		TurnMetadata:         cfg.TurnMetadata,
		IncludeTimingMetrics: false,
		Originator:           cfg.WireIdentity.Originator,
		UserAgent:            cfg.WireIdentity.UserAgent,
		OpenAIBeta:           cfg.WireIdentity.OpenAIBeta,
		Attestation:          cfg.WireIdentity.Attestation,
		StripWireFlags:       cfg.StripWireFlags,
	})
	// Strip the websocket-only OpenAI-Beta upgrade-version header. The
	// WS header builder Set()s it under its canonical form, so deleting
	// the canonicalized key removes it.
	delete(header, http.CanonicalHeaderKey(openAIBetaHeader))
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "text/event-stream")
	return header
}

// runHTTPTransportEvents POSTs one Codex Responses request over HTTP and
// streams the SSE reply through the shared codex SSE parser, emitting
// the same adapterrender.Event stream as the websocket transport.
func runHTTPTransportEvents(
	ctx context.Context,
	cfg HTTPTransportConfig,
	payload HTTPTransportRequest,
	emit func(adapterrender.Event) error,
) (RunResult, error) {
	return runHTTPTransportEventsWithRetry(ctx, cfg, payload, emit, adapterretry.Sleep)
}

func runHTTPTransportEventsWithRetry(
	ctx context.Context,
	cfg HTTPTransportConfig,
	payload HTTPTransportRequest,
	emit func(adapterrender.Event) error,
	sleep adapterretry.Sleeper,
) (RunResult, error) {
	if sleep == nil {
		sleep = adapterretry.Sleep
	}
	attempt := 1
	for {
		attemptCtx := ctx
		releaseAttempt := func(string) {}
		if cfg.BeforeAttempt != nil {
			attemptCtx, releaseAttempt = cfg.BeforeAttempt(ctx, attempt)
		}
		result, responseStarted, err := runHTTPTransportEventsOnce(attemptCtx, cfg, payload, emit)
		if err == nil {
			releaseAttempt("codex.http.attempt.success")
			return result, nil
		}
		if IsPermanentRefreshFailure(err) {
			// Auth refresh failed permanently (refresh token expired,
			// reused, or revoked). Retry cannot recover; bypass the
			// policy and surface the diagnostic to the user.
			releaseAttempt("codex.http.attempt.auth_permanent")
			return result, err
		}
		releaseAttempt("codex.http.attempt.failed")
		signal := adapterretry.Signal{
			Backend:         "codex",
			Operation:       codexHTTPRetryOperation,
			Status:          0,
			ErrorClass:      codexRetryErrorClass(err),
			ErrorCode:       "",
			Message:         err.Error(),
			ResponseStarted: responseStarted,
		}
		decision := adapterretry.Decide(cfg.RetryPolicies, signal, attempt, nil)
		maxAttempts := adapterretry.MaxAttempts(cfg.RetryPolicies, decision.PolicyName)
		if !decision.Retry {
			logHTTPRetryDecision(ctx, cfg, decision, attempt, maxAttempts, "failed")
			return result, err
		}
		logHTTPRetryDecision(ctx, cfg, decision, attempt, maxAttempts, "retrying")
		if err := sleep(ctx, decision.Delay); err != nil {
			return result, err
		}
		attempt++
	}
}

func runHTTPTransportEventsOnce(
	ctx context.Context,
	cfg HTTPTransportConfig,
	payload HTTPTransportRequest,
	emit func(adapterrender.Event) error,
) (RunResult, bool, error) {
	started := clock.Now()
	// The Codex Responses HTTP endpoint requires stream:true so it
	// replies with a text/event-stream body. Reference:
	// research/codex/codex-rs/core/src/client.rs ResponsesApiRequest.
	payload.Stream = true
	body, err := json.Marshal(payload)
	if err != nil {
		wrapped := fmt.Errorf("codex http transport: marshal request: %w", err)
		logHTTPTransportError(ctx, cfg, "marshal_request", wrapped)
		return NewRunResult("stop"), false, wrapped
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		wrapped := fmt.Errorf("codex http transport: build request: %w", err)
		logHTTPTransportError(ctx, cfg, "build_request", wrapped)
		return NewRunResult("stop"), false, wrapped
	}
	req.Header = buildResponsesHTTPHeaders(cfg)

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		wrapped := fmt.Errorf("codex http transport: %w", err)
		logHTTPTransportError(ctx, cfg, "http_request_failed", wrapped)
		return NewRunResult("stop"), false, wrapped
	}

	// On a 401 or 403, refresh the auth token once and retry the
	// request. This catches the case where the cached access_token
	// went stale between Token() and the dial. At most one refresh
	// per HTTP request; the outer retry loop handles anything beyond.
	if shouldRefreshAuthForStatus(resp.StatusCode, cfg.AuthRefresh) {
		refreshedResp, refreshedCfg, refreshErr := retryHTTPTransportAfterAuthRefresh(ctx, cfg, body, resp, httpClient)
		if refreshErr != nil {
			return NewRunResult("stop"), false, refreshErr
		}
		resp = refreshedResp
		cfg = refreshedCfg
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status is folded into the error message rather than
		// returned as a status code: the codex provider error boundary
		// (codexProviderAdapterError) always maps codex failures to
		// HTTP 400 + invalid_request_error + upstream_* so Cursor never
		// sees a raw 5xx or 429 and keeps the diagnostic message.
		snippet := readHTTPErrorSnippet(resp.Body)
		// The error body snippet is persisted to the capture store and surfaced
		// to the client through statusErr, but kept out of the log; the log
		// records only the upstream status so no response body lands in JSONL.
		logHTTPTransportError(ctx, cfg, "upstream_non_200", fmt.Errorf("codex http transport: upstream status %d", resp.StatusCode))
		statusErr := fmt.Errorf("codex http transport: upstream status %d: %s", resp.StatusCode, snippet)
		recordCodexHTTPEgress(cfg.CaptureStore, cfg.Correlation, req, resp, body, []byte(snippet), cfg.ConversationID, started)
		return NewRunResult("stop"), false, statusErr
	}

	logCtx := sseInstrumentationContext{
		RequestID:          cfg.RequestID,
		CursorRequestID:    cfg.CursorRequestID,
		ConversationID:     cfg.ConversationID,
		Correlation:        cfg.Correlation,
		Alias:              cfg.Alias,
		Model:              payload.Model,
		Transport:          "responses_http",
		ServiceTier:        payload.ServiceTier,
		PromptCacheKey:     payload.PromptCache,
		PreviousResponseID: "",
		Warmup:             false,
	}
	parseOpts := SSEParseOptions{DropEncryptedContent: cfg.RoundTripEncrypted == RoundTripEncryptedDrop, DeclaredTools: payload.Tools}
	responseStarted := false
	sink := capture.NewCappedBuffer(codexCaptureBodyCap)
	var bodyReader io.Reader = resp.Body
	if cfg.CaptureStore != nil {
		bodyReader = io.TeeReader(resp.Body, sink)
	}
	result, parseErr := ParseSSEEventsWithOptions(ctx, bodyReader, func(event adapterrender.Event) error {
		if codexRenderEventStartsClientResponse(event) {
			responseStarted = true
		}
		return emit(event)
	}, logCtx, parseOpts)
	recordCodexHTTPEgress(cfg.CaptureStore, cfg.Correlation, req, resp, body, sink.Bytes(), cfg.ConversationID, started)
	return result, responseStarted, parseErr
}

func shouldRefreshAuthForStatus(status int, refresh func(context.Context) (string, error)) bool {
	if refresh == nil {
		return false
	}
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func retryHTTPTransportAfterAuthRefresh(
	ctx context.Context,
	cfg HTTPTransportConfig,
	body []byte,
	originalResp *http.Response,
	httpClient *http.Client,
) (*http.Response, HTTPTransportConfig, error) {
	_, _ = io.Copy(io.Discard, originalResp.Body)
	_ = originalResp.Body.Close()
	newToken, refreshErr := cfg.AuthRefresh(ctx)
	if refreshErr != nil {
		logHTTPTransportError(ctx, cfg, "auth_refresh_failed", refreshErr)
		return nil, cfg, refreshErr
	}
	if strings.TrimSpace(newToken) == "" {
		return originalResp, cfg, nil
	}
	cfg.Token = newToken
	retryReq, retryErr := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if retryErr != nil {
		wrapped := fmt.Errorf("codex http transport: build retry request: %w", retryErr)
		logHTTPTransportError(ctx, cfg, "build_retry_request", wrapped)
		return nil, cfg, wrapped
	}
	retryReq.Header = buildResponsesHTTPHeaders(cfg)
	retryResp, retryDoErr := httpClient.Do(retryReq)
	if retryDoErr != nil {
		wrapped := fmt.Errorf("codex http transport (post-refresh): %w", retryDoErr)
		logHTTPTransportError(ctx, cfg, "http_retry_failed", wrapped)
		return nil, cfg, wrapped
	}
	return retryResp, cfg, nil
}

func readHTTPErrorSnippet(r io.Reader) string {
	buf, _ := io.ReadAll(io.LimitReader(r, httpErrorBodySnippetLimit))
	snippet := strings.TrimSpace(string(buf))
	if snippet == "" {
		return "(empty body)"
	}
	return snippet
}

func logHTTPTransportError(ctx context.Context, cfg HTTPTransportConfig, stage string, err error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	log.WarnContext(ctx, "adapter.codex.http_transport_error", "concern", "adapter.providers.codex.request", "component", "adapter",
		"subcomponent", "codex",
		"request_id", cfg.RequestID,
		"trace_id", string(cfg.Correlation.TraceID),
		"stage", stage,
		"err", err.Error(),
	)
}

func logHTTPRetryDecision(ctx context.Context, cfg HTTPTransportConfig, decision adapterretry.Decision, attempt int, maxAttempts int, finalOutcome string) {
	adapterretry.LogDecision(ctx, cfg.Log, decision, attempt, maxAttempts, adapterretry.AttemptLogContext{
		RequestID: cfg.RequestID,
		TraceID:   string(cfg.Correlation.TraceID),
		ChatKey:   clydeingress.ChatKey(cfg.Correlation),
		Operation: codexHTTPRetryOperation,
	}, finalOutcome)
}
