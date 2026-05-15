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
	"goodkind.io/clyde/internal/correlation"
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
		BetaFeatures:         "",
		TurnState:            NewTurnState(),
		TurnMetadata:         cfg.TurnMetadata,
		IncludeTimingMetrics: false,
		Originator:           "",
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status is folded into the error message rather than
		// returned as a status code: the codex provider error boundary
		// (codexProviderAdapterError) always maps codex failures to
		// HTTP 400 + invalid_request_error + upstream_* so Cursor never
		// sees a raw 5xx or 429 and keeps the diagnostic message.
		snippet := readHTTPErrorSnippet(resp.Body)
		statusErr := fmt.Errorf("codex http transport: upstream status %d: %s", resp.StatusCode, snippet)
		logHTTPTransportError(ctx, cfg, "upstream_non_200", statusErr)
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
	parseOpts := SSEParseOptions{DropEncryptedContent: cfg.RoundTripEncrypted == RoundTripEncryptedDrop}
	responseStarted := false
	result, parseErr := ParseSSEEventsWithOptions(ctx, resp.Body, func(event adapterrender.Event) error {
		if codexRenderEventStartsClientResponse(event) {
			responseStarted = true
		}
		return emit(event)
	}, logCtx, parseOpts)
	return result, responseStarted, parseErr
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
	log.WarnContext(ctx, "adapter.codex.http_transport_error",
		"component", "adapter",
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
		ChatKey:   cfg.Correlation.ChatKey,
		Operation: codexHTTPRetryOperation,
	}, finalOutcome)
}
