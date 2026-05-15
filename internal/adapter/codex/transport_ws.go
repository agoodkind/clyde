package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterretry "goodkind.io/clyde/internal/adapter/retry"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/slogger"
)

type ResponseCreateClientMetadata map[string]string

type ResponseCreateWsRequest struct {
	Type               string                       `json:"type"`
	Model              string                       `json:"model,omitempty"`
	Instructions       string                       `json:"instructions,omitempty"`
	Input              []map[string]any             `json:"input,omitempty"`
	Tools              []any                        `json:"tools,omitempty"`
	ToolChoice         string                       `json:"tool_choice,omitempty"`
	ParallelToolCalls  bool                         `json:"parallel_tool_calls,omitempty"`
	Reasoning          *Reasoning                   `json:"reasoning,omitempty"`
	Store              bool                         `json:"store"`
	Stream             bool                         `json:"stream"`
	Include            []string                     `json:"include,omitempty"`
	ServiceTier        string                       `json:"service_tier,omitempty"`
	PromptCacheKey     string                       `json:"prompt_cache_key,omitempty"`
	Text               any                          `json:"text,omitempty"`
	ClientMetadata     ResponseCreateClientMetadata `json:"client_metadata,omitempty"`
	PreviousResponseID string                       `json:"previous_response_id,omitempty"`
	Generate           *bool                        `json:"generate,omitempty"`
}

var ErrWebsocketFallbackToHTTP = errors.New("codex websocket fallback to http")

// websocketHandshakeError carries the HTTP status from a failed
// Responses websocket upgrade. gorilla/websocket collapses every
// non-101 upgrade response into a bare "bad handshake" error and
// discards the status, so callers could not tell a 401 from a 503.
// Capturing the status keeps the failure observable in logs and lets
// the retry classifier reason about it.
type websocketHandshakeError struct {
	Status int
	Err    error
}

func (e *websocketHandshakeError) Error() string {
	return fmt.Sprintf("websocket handshake failed: status %d: %v", e.Status, e.Err)
}

func (e *websocketHandshakeError) Unwrap() error { return e.Err }

const defaultWebsocketPrewarmTimeout = 1500 * time.Millisecond

func ResponseCreateRequestFromHTTP(req HTTPTransportRequest) ResponseCreateWsRequest {
	return ResponseCreateWsRequest{
		Type:              "response.create",
		Model:             req.Model,
		Instructions:      req.Instructions,
		Input:             req.Input,
		Tools:             req.Tools,
		ToolChoice:        req.ToolChoice,
		ParallelToolCalls: req.ParallelToolCalls,
		Reasoning:         req.Reasoning,
		Store:             req.Store,
		Stream:            req.Stream,
		Include:           req.Include,
		ServiceTier:       req.ServiceTier,
		PromptCacheKey:    req.PromptCache,
		Text:              req.Text,
		ClientMetadata:    ResponseCreateClientMetadata(req.ClientMetadata),
	}
}

func WithWarmupGenerateFalse(req ResponseCreateWsRequest) ResponseCreateWsRequest {
	generate := false
	req.Generate = &generate
	return req
}

func WithPreviousResponseID(req ResponseCreateWsRequest, previousResponseID string, incrementalInput []map[string]any) ResponseCreateWsRequest {
	req.PreviousResponseID = previousResponseID
	if incrementalInput != nil {
		req.Input = incrementalInput
	}
	return req
}

func MarshalResponseCreateWsRequest(req ResponseCreateWsRequest) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	// Force `input: []` whenever the request semantically carries an
	// empty input but the field would otherwise be omitted by
	// json:omitempty. The upstream rejects the frame with
	// "Missing required parameter: 'input'" when the field is
	// absent. Cases:
	//   - Warmup (Generate == false): always sends empty input.
	//   - Continuation (PreviousResponseID set, no new items): the
	//     prior response's items are server-side; we send no delta.
	isWarmup := req.Generate != nil && !*req.Generate
	forceEmptyInput := req.Input != nil && len(req.Input) == 0 &&
		(isWarmup || req.PreviousResponseID != "")
	forceEmptyTools := isWarmup && req.Tools != nil && len(req.Tools) == 0
	if !forceEmptyInput && !forceEmptyTools {
		return raw, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if forceEmptyInput {
		payload["input"] = []map[string]any{}
	}
	if forceEmptyTools {
		payload["tools"] = []any{}
	}
	return json.Marshal(payload)
}

type WebsocketTransportConfig struct {
	URL             string
	Token           string
	AccountID       string
	RequestID       string
	CursorRequestID string
	Correlation     correlation.Context
	Alias           string
	ConversationID  string
	TurnState       *TurnState
	TurnMetadata    string
	Prewarm         bool
	PrewarmTimeout  time.Duration
	BodyLog         BodyLogConfig
	BodyLogProvider BodyLogConfigProvider

	// SessionCache enables persistent ws session reuse when set. The
	// transport takes the cached session for ConversationID, sends a
	// delta payload referencing the cached LastResponseID, then puts
	// the session back on success. When nil or ConversationID is
	// empty, the transport falls back to the legacy fresh-dial path
	// that warms up and closes per call.
	SessionCache *WebsocketSessionCache
	// Log carries ws_session telemetry events. Optional; falls back
	// to slog.Default().
	Log *slog.Logger
	// WireCaptureMode controls per-frame body capture. Off (default)
	// emits nothing. SummaryOnly emits a fingerprint per frame.
	// ReasoningOnly emits the body only on response.output_item.done
	// frames carrying a reasoning item. Full emits the body on every
	// inbound frame. Routed to adapter.providers.codex.wire_capture.
	WireCaptureMode WireCaptureMode
	// RoundTripEncrypted controls whether the SSE parser surfaces the
	// encrypted_content blob from completed reasoning items on
	// EventReasoningFinished. RoundTripEncryptedRoundTrip (the
	// codex-rs default) keeps the blob; RoundTripEncryptedDrop strips
	// it so the synthetic-thinking close marker stays bare. Empty
	// resolves to RoundTripEncryptedRoundTrip.
	RoundTripEncrypted RoundTripEncrypted
	// RetryPolicies are generic adapter retry rules compiled at daemon startup.
	RetryPolicies []adapterretry.Policy
	// BeforeAttempt, when non-nil, is called at the start of every
	// retry attempt (one-based attempt number). It returns a
	// (possibly derived) context to use for the attempt and a release
	// function the transport calls when the attempt ends. The caller
	// (adapter.Server) uses this to register each attempt as a nested
	// livetrack egress session without importing the adapter package
	// from here.
	BeforeAttempt func(ctx context.Context, attemptNo int) (context.Context, func(string))
	// AuthRefresh, when non-nil, is invoked when the websocket upgrade
	// responds with HTTP 401 or 403. It returns a refreshed access
	// token (or an error if the refresh itself failed) so the dial can
	// retry once with the new token before propagating the failure.
	AuthRefresh func(ctx context.Context) (string, error)
}

// Mirrors the observed Responses websocket envelope from
// research/codex/scripts/mock_responses_websocket_server.py.
type websocketEventEnvelope struct {
	Type  string                 `json:"type"`
	Error *websocketErrorPayload `json:"error,omitempty"`
}

type websocketErrorPayload struct {
	Message string `json:"message,omitempty"`
}

func websocketMessageToSyntheticSSE(message []byte) ([]byte, error) {
	var raw websocketEventEnvelope
	if err := json.Unmarshal(message, &raw); err != nil {
		return nil, err
	}
	kind := strings.TrimSpace(raw.Type)
	if kind == "" {
		return nil, fmt.Errorf("codex websocket message missing type")
	}
	if kind == "error" {
		msg := "codex websocket error"
		if raw.Error != nil && strings.TrimSpace(raw.Error.Message) != "" {
			msg = raw.Error.Message
		}
		return nil, codexResponseFailedError(msg)
	}
	var b bytes.Buffer
	_, _ = fmt.Fprintf(&b, "event: %s\n", kind)
	_, _ = fmt.Fprintf(&b, "data: %s\n\n", bytes.TrimSpace(message))
	return b.Bytes(), nil
}

func streamWebsocketAsSyntheticSSE(ctx context.Context, conn *websocket.Conn, logCtx sseInstrumentationContext, wireMode WireCaptureMode) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Default().Error("adapter.codex.websocket_reader_panic",
					"component", "adapter",
					"subcomponent", "codex",
					"err", fmt.Sprintf("panic: %v", recovered),
					"panic", recovered,
				)
				_ = pw.CloseWithError(fmt.Errorf("codex websocket reader panic: %v", recovered))
				return
			}
			_ = pw.Close()
		}()
		seq := 0
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if messageType != websocket.TextMessage {
				continue
			}
			seq++
			frame, err := websocketMessageToSyntheticSSE(message)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(frame); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			var raw websocketEventEnvelope
			parseOK := json.Unmarshal(message, &raw) == nil
			if wireMode != "" && wireMode != WireCaptureOff {
				emitCodexWireCapture(ctx, wireMode, logCtx, seq, raw, message)
			}
			if parseOK && (raw.Type == "response.completed" || raw.Type == "response.failed") {
				return
			}
		}
	}()
	return pr
}

// codexReasoningItemDone reports whether the inbound frame is a
// response.output_item.done event whose `item.type` is `reasoning`. This is
// the cheapest predicate for proving encrypted_content arrival on the wire.
func codexReasoningItemDone(message []byte) bool {
	var probe struct {
		Type string `json:"type"`
		Item struct {
			Type string `json:"type"`
		} `json:"item"`
	}
	if err := json.Unmarshal(message, &probe); err != nil {
		return false
	}
	return probe.Type == "response.output_item.done" && probe.Item.Type == "reasoning"
}

// emitCodexWireCapture routes one inbound frame to the
// adapter.providers.codex.wire_capture concern according to mode. Off is
// filtered by the caller. SummaryOnly emits a fingerprint with no body.
// ReasoningOnly emits the body only on response.output_item.done frames
// carrying a reasoning item. Full emits the body unconditionally. The body
// is emitted as a [json.RawMessage] so the upstream JSON shape is preserved
// without escaping.
func emitCodexWireCapture(ctx context.Context, mode WireCaptureMode, logCtx sseInstrumentationContext, seq int, raw websocketEventEnvelope, message []byte) {
	includeBody := false
	switch mode {
	case WireCaptureOff:
		return
	case WireCaptureSummaryOnly:
		includeBody = false
	case WireCaptureReasoningOnly:
		includeBody = codexReasoningItemDone(message)
		if !includeBody {
			return
		}
	case WireCaptureFull:
		includeBody = true
	}
	attrs := []slog.Attr{
		slog.String("subcomponent", "codex"),
		slog.String("mode", string(mode)),
		slog.String("request_id", logCtx.RequestID),
		slog.String("cursor_request_id", logCtx.CursorRequestID),
		slog.String("conversation_id", logCtx.ConversationID),
		slog.String("alias", logCtx.Alias),
		slog.String("model", logCtx.Model),
		slog.String("upstream_event_type", raw.Type),
		slog.Int("frame_seq", seq),
		slog.Int("payload_bytes", len(message)),
	}
	if includeBody {
		attrs = append(attrs, slog.Any("body", json.RawMessage(message)))
	}
	codexWireCaptureLog.Logger().LogAttrs(ctx, slog.LevelInfo, "adapter.providers.codex.wire_capture", attrs...)
}

func writeAndParseWebsocketRequest(
	ctx context.Context,
	conn *websocket.Conn,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapterrender.Event) error,
	warmup bool,
) (RunResult, bool, error) {
	raw, err := MarshalResponseCreateWsRequest(payload)
	if err != nil {
		return NewRunResult("stop"), false, err
	}
	logWebsocketFrame(ctx, cfg, payload, raw, warmup)
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		return NewRunResult("stop"), false, err
	}
	logCtx := sseInstrumentationContext{
		RequestID:          cfg.RequestID,
		CursorRequestID:    cfg.CursorRequestID,
		ConversationID:     cfg.ConversationID,
		Correlation:        cfg.Correlation,
		Alias:              cfg.Alias,
		Model:              payload.Model,
		Transport:          "responses_websocket",
		ServiceTier:        payload.ServiceTier,
		PromptCacheKey:     payload.PromptCacheKey,
		PreviousResponseID: payload.PreviousResponseID,
		Warmup:             warmup,
	}
	synthetic := streamWebsocketAsSyntheticSSE(ctx, conn, logCtx, cfg.WireCaptureMode)
	parseOpts := SSEParseOptions{DropEncryptedContent: cfg.RoundTripEncrypted == RoundTripEncryptedDrop}
	responseStarted := false
	result, err := ParseSSEEventsWithOptions(ctx, synthetic, func(event adapterrender.Event) error {
		if codexRenderEventStartsClientResponse(event) {
			responseStarted = true
		}
		return emit(event)
	}, logCtx, parseOpts)
	if err == nil || strings.TrimSpace(result.ResponseID) != "" || result.UsageTelemetry.UsagePresent {
		LogUsageTelemetry(ctx, cfg.Log, result.UsageTelemetry, CodexUsageLogContext{
			RequestID:          cfg.RequestID,
			CursorRequestID:    cfg.CursorRequestID,
			Correlation:        cfg.Correlation,
			Alias:              cfg.Alias,
			UpstreamModel:      payload.Model,
			Transport:          "responses_websocket",
			ServiceTier:        payload.ServiceTier,
			PromptCacheKey:     payload.PromptCacheKey,
			PreviousResponseID: payload.PreviousResponseID,
			ResponseID:         result.ResponseID,
			ConversationID:     cfg.ConversationID,
			WebsocketWarmup:    warmup,
		})
	}
	if strings.TrimSpace(result.ResponseID) != "" {
		corr := cfg.Correlation.WithUpstreamResponseID(result.ResponseID)
		attrs := []slog.Attr{
			slog.String("component", "adapter"),
			slog.String("subcomponent", "codex"),
			slog.String("request_id", cfg.RequestID),
			slog.String("conversation_id", cfg.ConversationID),
			slog.Bool("warmup", warmup),
		}
		attrs = append(attrs, corr.Attrs()...)
		logCodexEvent(ctx, slog.LevelInfo, "adapter.codex.response.received", attrs)
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return result, responseStarted, nil
	}
	return result, responseStarted, err
}

func codexRenderEventStartsClientResponse(event adapterrender.Event) bool {
	switch event.(type) {
	case adapterrender.TextDelta,
		adapterrender.RefusalDelta,
		adapterrender.ReasoningSignaled,
		adapterrender.ReasoningDelta,
		adapterrender.ToolCallDelta:
		return true
	default:
		return false
	}
}

// logWebsocketFrame emits codex.responses.request for every websocket
// frame Clyde writes (warmup and primary). The frame bytes are exactly
// what the wire receives, so corruption between BuildRequest and the
// websocket write is observable in the JSONL feed.
func logWebsocketFrame(ctx context.Context, cfg WebsocketTransportConfig, payload ResponseCreateWsRequest, frame []byte, warmup bool) {
	if !warmup {
		summary := summarizeFinalResponseCreateFrame(cfg, payload, frame)
		logCodexEventWithConcern(ctx, slog.LevelInfo, "adapter.codex.response_create_frame.summary", slogger.ConcernAdapterProviderCodexWS, summary.toSlogAttrs())
	}
	mode, maxBytes := resolveBodyLogConfig(cfg.BodyLog, cfg.BodyLogProvider).Resolve()
	if mode == BodyLogOff {
		return
	}
	ev := requestEvent{
		Subcomponent:       "codex",
		Transport:          "responses_websocket",
		RequestID:          cfg.RequestID,
		CursorRequestID:    cfg.CursorRequestID,
		Correlation:        cfg.Correlation,
		Alias:              cfg.Alias,
		Model:              payload.Model,
		URL:                cfg.URL,
		BodyBytes:          len(frame),
		InputCount:         len(payload.Input),
		ToolCount:          len(payload.Tools),
		PreviousResponseID: payload.PreviousResponseID,
		Warmup:             warmup,
	}
	body, truncated := applyBodyMode(frame, mode, maxBytes)
	ev.Body = body
	ev.BodyTruncated = truncated
	if mode == BodyLogSummary || mode == BodyLogWhitelist {
		ev.BodySummary = summarizeWsRequest(payload)
	}
	logCodexEvent(ctx, slog.LevelDebug, "codex.responses.request", ev.toSlogAttrs())
}

func dialResponsesWebsocket(ctx context.Context, cfg WebsocketTransportConfig) (*websocket.Conn, int, error) {
	dialer := websocket.Dialer{}
	installationID, _ := LoadInstallationID()
	turnMetadataJSON := strings.TrimSpace(cfg.TurnMetadata)
	conv := strings.TrimSpace(cfg.ConversationID)
	if conv != "" && turnMetadataJSON == "" {
		if json, err := NewTurnMetadata(conv, "").MarshalCompact(); err == nil {
			turnMetadataJSON = json
		}
	}
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:      cfg.RequestID,
		ConversationID: cfg.ConversationID,
		Correlation:    cfg.Correlation,
		Token:          cfg.Token,
		InstallationID: installationID,
		TurnState:      cfg.TurnState,
		TurnMetadata:   turnMetadataJSON,
	})
	conn, resp, err := dialer.DialContext(ctx, cfg.URL, header)
	statusCode := 0
	if resp != nil && cfg.TurnState != nil {
		cfg.TurnState.CaptureFromHeaders(resp.Header)
	}
	if resp != nil {
		statusCode = resp.StatusCode
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil && statusCode != 0 {
		return conn, statusCode, &websocketHandshakeError{Status: statusCode, Err: err}
	}
	return conn, statusCode, err
}

func logWebsocketPrepared(ctx context.Context, cfg WebsocketTransportConfig, payload ResponseCreateWsRequest, telemetry TransportTelemetry) {
	telemetry.RequestID = cfg.RequestID
	telemetry.CursorRequestID = cfg.CursorRequestID
	telemetry.Correlation = cfg.Correlation
	telemetry.Alias = cfg.Alias
	telemetry.UpstreamModel = payload.Model
	telemetry.Transport = "responses_websocket"
	telemetry.ServiceTier = payload.ServiceTier
	telemetry.PromptCacheKey = payload.PromptCacheKey
	telemetry.ClientMetadata = map[string]string(payload.ClientMetadata)
	telemetry.InputCount = len(payload.Input)
	telemetry.ToolCount = len(payload.Tools)
	telemetry.PreviousResponseID = payload.PreviousResponseID
	telemetry.TurnStatePresent = cfg.TurnState.Value() != ""
	LogTransportPrepared(ctx, nil, telemetry)
}

func RunWebsocketTransportEvents(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapterrender.Event) error,
) (RunResult, error) {
	return runWebsocketTransportEventsWithRetry(ctx, cfg, payload, emit, adapterretry.Sleep)
}

func runWebsocketTransportEventsWithRetry(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapterrender.Event) error,
	sleep adapterretry.Sleeper,
) (RunResult, error) {
	if sleep == nil {
		sleep = adapterretry.Sleep
	}
	operation := codexWebsocketRetryOperation
	attempt := 1
	lastPolicyName := ""
	lastMaxAttempts := 0
	for {
		// Register each attempt as a nested egress session when the
		// caller supplied a BeforeAttempt hook. The hook supplies a
		// possibly-derived context (e.g. with a cancel tied to livetrack
		// force-close) and a release function to call when the attempt
		// ends. When the hook is nil the original ctx and a no-op
		// release are used so the retry loop is unconditionally safe.
		attemptCtx := ctx
		releaseAttempt := func(string) {}
		if cfg.BeforeAttempt != nil {
			attemptCtx, releaseAttempt = cfg.BeforeAttempt(ctx, attempt)
		}
		result, responseStarted, err := runWebsocketTransportEventsOnce(attemptCtx, cfg, payload, emit)
		if err == nil {
			releaseAttempt("codex.attempt.success")
			logCodexRetryTerminal(ctx, cfg, attempt, lastPolicyName, "success", lastMaxAttempts)
			return result, nil
		}
		if errors.Is(err, ErrWebsocketFallbackToHTTP) {
			// The upstream asked for the HTTP transport (HTTP 426). This
			// is a transport switch, not a retryable failure, so it must
			// propagate to RunDirect unconsumed by the retry policy.
			releaseAttempt("codex.attempt.fallback_http")
			return result, err
		}
		if IsPermanentRefreshFailure(err) {
			// The auth refresh itself failed permanently (refresh token
			// expired, reused, or revoked). Retry cannot recover, so
			// bypass the policy and surface the error immediately so the
			// user sees the diagnostic instead of three identical
			// attempts.
			releaseAttempt("codex.attempt.auth_permanent")
			return result, err
		}
		releaseAttempt("codex.attempt.failed")
		signal := adapterretry.Signal{
			Backend:         "codex",
			Operation:       operation,
			Status:          0,
			ErrorClass:      codexRetryErrorClass(err),
			ErrorCode:       "",
			Message:         err.Error(),
			ResponseStarted: responseStarted,
		}
		decision := adapterretry.Decide(cfg.RetryPolicies, signal, attempt, nil)
		if !decision.Retry {
			maxAttempts := adapterretry.MaxAttempts(cfg.RetryPolicies, decision.PolicyName)
			logCodexRetryDecision(ctx, cfg, decision, attempt, maxAttempts, "failed")
			return result, err
		}
		maxAttempts := adapterretry.MaxAttempts(cfg.RetryPolicies, decision.PolicyName)
		logCodexRetryDecision(ctx, cfg, decision, attempt, maxAttempts, "retrying")
		lastPolicyName = decision.PolicyName
		lastMaxAttempts = maxAttempts
		if err := sleep(ctx, decision.Delay); err != nil {
			return result, err
		}
		attempt++
	}
}

func runWebsocketTransportEventsOnce(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapterrender.Event) error,
) (RunResult, bool, error) {
	if cfg.SessionCache != nil && strings.TrimSpace(cfg.ConversationID) != "" {
		return runWebsocketWithCache(ctx, cfg, payload, emit)
	}
	return runWebsocketFreshDial(ctx, cfg, payload, emit)
}

func codexRetryErrorClass(err error) string {
	if err == nil {
		return ""
	}
	var handshakeErr *websocketHandshakeError
	if errors.As(err, &handshakeErr) {
		// Every handshake failure (401, 403, 429, 5xx, or a bare bad
		// handshake) surfaces before any response bytes are emitted, so
		// it is safe to retry. The 401 and 403 cases also trigger one
		// auth-refresh-and-redial inside the dial wrapper before
		// reaching the retry policy; a token that is still rejected
		// after refresh classifies as response_failed here and exhausts
		// the bounded retry budget.
		return "response_failed"
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return "websocket_close"
	}
	if strings.Contains(err.Error(), "codex websocket") {
		return "websocket_error"
	}
	return "response_failed"
}

// dialResponsesWebsocketWithAuthRefresh wraps dialResponsesWebsocket so
// that a 401 or 403 on the upgrade triggers one auth refresh and a
// re-dial with the refreshed token. The refresh runs at most once per
// call. A successful refresh that produces an empty token, or a refresh
// that itself fails, propagates the original handshake error wrapped
// with the refresh outcome so the retry loop can decide what to do.
// Returns the (possibly updated) cfg so the caller's later dials see
// the new token.
func dialResponsesWebsocketWithAuthRefresh(ctx context.Context, cfg WebsocketTransportConfig) (*websocket.Conn, int, WebsocketTransportConfig, error) {
	conn, statusCode, err := dialResponsesWebsocket(ctx, cfg)
	if err == nil || cfg.AuthRefresh == nil {
		return conn, statusCode, cfg, err
	}
	var handshakeErr *websocketHandshakeError
	if !errors.As(err, &handshakeErr) {
		return conn, statusCode, cfg, err
	}
	if handshakeErr.Status != http.StatusUnauthorized && handshakeErr.Status != http.StatusForbidden {
		return conn, statusCode, cfg, err
	}
	newToken, refreshErr := cfg.AuthRefresh(ctx)
	if refreshErr != nil {
		return conn, statusCode, cfg, refreshErr
	}
	if strings.TrimSpace(newToken) == "" {
		return conn, statusCode, cfg, err
	}
	cfg.Token = newToken
	return dialResponsesWebsocketAfterRefresh(ctx, cfg)
}

func dialResponsesWebsocketAfterRefresh(ctx context.Context, cfg WebsocketTransportConfig) (*websocket.Conn, int, WebsocketTransportConfig, error) {
	conn, statusCode, err := dialResponsesWebsocket(ctx, cfg)
	return conn, statusCode, cfg, err
}

func logCodexRetryDecision(ctx context.Context, cfg WebsocketTransportConfig, decision adapterretry.Decision, attempt int, maxAttempts int, finalOutcome string) {
	adapterretry.LogDecision(ctx, cfg.Log, decision, attempt, maxAttempts, adapterretry.AttemptLogContext{
		RequestID: cfg.RequestID,
		TraceID:   string(cfg.Correlation.TraceID),
		ChatKey:   cfg.Correlation.ChatKey,
		Operation: codexWebsocketRetryOperation,
	}, finalOutcome)
}

func logCodexRetryTerminal(ctx context.Context, cfg WebsocketTransportConfig, attempt int, policyName string, finalOutcome string, maxAttempts int) {
	if attempt <= 1 {
		return
	}
	logCodexRetryDecision(ctx, cfg, adapterretry.Decision{
		Retry:      false,
		PolicyName: policyName,
		Delay:      0,
		Reason:     "operation_succeeded",
	}, attempt, maxAttempts, finalOutcome)
}

// runWebsocketFreshDial is the legacy path. Dial a fresh websocket,
// optionally warm up, send one frame, close. Preserved so tests and
// non-cache callers do not break. Tagged for removal once all
// callers route through runWebsocketWithCache.
func runWebsocketFreshDial(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapterrender.Event) error,
) (RunResult, bool, error) {
	conn, statusCode, refreshedCfg, err := dialResponsesWebsocketWithAuthRefresh(ctx, cfg)
	cfg = refreshedCfg
	if statusCode == http.StatusUpgradeRequired {
		logWebsocketPrepared(ctx, cfg, payload, TransportTelemetry{FallbackToHTTP: true})
		return NewRunResult("stop"), false, ErrWebsocketFallbackToHTTP
	}
	if err != nil {
		return NewRunResult("stop"), false, err
	}
	defer func(c *websocket.Conn) { _ = c.Close() }(conn)

	prewarmUsed := false
	prewarmFailed := false
	connectionReused := false
	if cfg.Prewarm && strings.TrimSpace(payload.PreviousResponseID) == "" {
		warmup := WithWarmupGenerateFalse(payload)
		warmup.Tools = []any{}
		logWebsocketPrepared(ctx, cfg, warmup, TransportTelemetry{WebsocketWarmup: true})
		prewarmTimeout := cfg.PrewarmTimeout
		if prewarmTimeout <= 0 {
			prewarmTimeout = defaultWebsocketPrewarmTimeout
		}
		_ = conn.SetReadDeadline(codexClock.Now().Add(prewarmTimeout))
		warmupResult, _, warmupErr := writeAndParseWebsocketRequest(ctx, conn, cfg, warmup, func(adapterrender.Event) error {
			return nil
		}, true)
		_ = conn.SetReadDeadline(time.Time{})
		if warmupErr == nil && strings.TrimSpace(warmupResult.ResponseID) != "" {
			payload = WithPreviousResponseID(payload, warmupResult.ResponseID, []map[string]any{})
			prewarmUsed = true
			connectionReused = true
		} else {
			prewarmFailed = true
			_ = conn.Close()
			var refreshedCfg WebsocketTransportConfig
			conn, statusCode, refreshedCfg, err = dialResponsesWebsocketWithAuthRefresh(ctx, cfg)
			cfg = refreshedCfg
			if statusCode == http.StatusUpgradeRequired {
				logWebsocketPrepared(ctx, cfg, payload, TransportTelemetry{
					FallbackToHTTP:         true,
					WebsocketPrewarmFailed: prewarmFailed,
				})
				return NewRunResult("stop"), false, ErrWebsocketFallbackToHTTP
			}
			if err != nil {
				return NewRunResult("stop"), false, err
			}
			defer func(c *websocket.Conn) { _ = c.Close() }(conn)
		}
	}

	logWebsocketPrepared(ctx, cfg, payload, TransportTelemetry{
		WebsocketPrewarmUsed:     prewarmUsed,
		WebsocketPrewarmFailed:   prewarmFailed,
		WebsocketConnectionReuse: connectionReused,
	})

	return writeAndParseWebsocketRequest(ctx, conn, cfg, payload, emit, false)
}

// runWebsocketWithCache implements the parity-superset path. The
// transport takes a cached session keyed on ConversationID, computes
// a delta of the input items relative to the prior baseline, sets
// previous_response_id from the cache entry, sends one frame, and
// returns the session to the cache on success. On any error the
// session is invalidated. Reference: codex-rs/core/src/client.rs
// stream_responses().
func runWebsocketWithCache(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapterrender.Event) error,
) (RunResult, bool, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	conv := strings.TrimSpace(cfg.ConversationID)
	fullInput := payload.Input

	session, hit := cfg.SessionCache.Take(ctx, conv)
	if hit {
		log.InfoContext(ctx, "adapter.codex.ws_session.taken",
			"component", "adapter",
			"subcomponent", "codex",
			"conversation_id", conv,
			"last_response_id", session.LastResponseID,
			"session_model", session.Model,
			"request_model", payload.Model,
			"frame_count", session.FrameCount,
			"age_ms", time.Since(session.OpenedAt).Milliseconds(),
		)
	}
	if hit && !websocketSessionCompatible(session, payload) {
		cfg.SessionCache.invalidateEntry(session, "model_mismatch")
		session = nil
		hit = false
	}
	if hit {
		delta := ComputeDelta(session.LastInputItems, fullInput)
		switch {
		case delta.Ok:
			payload = WithPreviousResponseID(payload, session.LastResponseID, delta.Items)
		case delta.Reason == "no_extension":
			cfg.SessionCache.invalidateEntry(session, "no_extension")
			session = nil
			hit = false
		default:
			cfg.SessionCache.invalidateEntry(session, delta.Reason)
			session = nil
			hit = false
		}
	}

	if !hit {
		opened, err := openSessionAndWarmup(ctx, cfg, payload, log)
		if err != nil {
			log.WarnContext(ctx, "adapter.codex.ws_session.warmup_fallback_uncached",
				"component", "adapter",
				"subcomponent", "codex",
				"conversation_id", conv,
				"request_id", cfg.RequestID,
				"err", err.Error(),
			)
			freshCfg := cfg
			freshCfg.SessionCache = nil
			freshCfg.Prewarm = false
			return runWebsocketFreshDial(ctx, freshCfg, payload, emit)
		}
		session = opened
		if strings.TrimSpace(session.LastResponseID) != "" {
			payload = WithPreviousResponseID(payload, session.LastResponseID, fullInput)
		}
	}

	logWebsocketPrepared(ctx, cfg, payload, TransportTelemetry{
		WebsocketConnectionReuse: hit,
	})
	log.InfoContext(ctx, "adapter.codex.frame.sent",
		"component", "adapter",
		"subcomponent", "codex",
		"conversation_id", conv,
		"request_id", cfg.RequestID,
		"prev_response_id", payload.PreviousResponseID,
		"delta_input_count", len(payload.Input),
		"full_input_count", len(fullInput),
		"is_warmup", false,
	)

	result, responseStarted, err := writeAndParseWebsocketRequest(ctx, session.Conn, cfg, payload, emit, false)
	if err != nil {
		cfg.SessionCache.Invalidate(ctx, conv, "ws_io_error")
		return result, responseStarted, err
	}

	session.LastResponseID = strings.TrimSpace(result.ResponseID)
	if session.LastResponseID == "" {
		// Server completed without an id. Drop the connection rather
		// than re-cache without a chain anchor.
		cfg.SessionCache.Invalidate(ctx, conv, "missing_response_id")
		return result, responseStarted, nil
	}
	session.Model = payload.Model
	session.PromptCacheKey = payload.PromptCacheKey
	session.LastInputItems = cloneInputItems(fullInput)
	session.FrameCount++
	cfg.SessionCache.Put(ctx, session)
	log.InfoContext(ctx, "adapter.codex.ws_session.put",
		"component", "adapter",
		"subcomponent", "codex",
		"conversation_id", conv,
		"last_response_id", session.LastResponseID,
		"frame_count", session.FrameCount,
	)
	return result, responseStarted, nil
}

// openSessionAndWarmup dials a fresh websocket, sends the warmup
// frame (generate=false, empty input, no prev), captures the
// response_id, and returns a populated WebsocketSession ready to
// carry a real frame. The caller is responsible for installing the
// session in the cache after the first real frame succeeds.
func openSessionAndWarmup(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	log *slog.Logger,
) (*WebsocketSession, error) {
	conv := strings.TrimSpace(cfg.ConversationID)
	conn, statusCode, _, err := dialResponsesWebsocketWithAuthRefresh(ctx, cfg)
	if statusCode == http.StatusUpgradeRequired {
		return nil, ErrWebsocketFallbackToHTTP
	}
	if err != nil {
		return nil, err
	}

	warmup := WithWarmupGenerateFalse(payload)
	warmup.Tools = []any{}
	warmup.Input = []map[string]any{}
	warmup.PreviousResponseID = ""
	prewarmTimeout := cfg.PrewarmTimeout
	if prewarmTimeout <= 0 {
		prewarmTimeout = defaultWebsocketPrewarmTimeout
	}
	_ = conn.SetReadDeadline(codexClock.Now().Add(prewarmTimeout))
	warmupResult, _, warmupErr := writeAndParseWebsocketRequest(ctx, conn, cfg, warmup, func(adapterrender.Event) error {
		return nil
	}, true)
	_ = conn.SetReadDeadline(time.Time{})
	if warmupErr != nil || strings.TrimSpace(warmupResult.ResponseID) == "" {
		_ = conn.Close()
		if warmupErr != nil {
			log.WarnContext(ctx, "adapter.codex.ws_session.warmup_failed",
				"component", "adapter",
				"subcomponent", "codex",
				"conversation_id", conv,
				"err", warmupErr.Error(),
			)
			return nil, fmt.Errorf("codex websocket warmup failed: %w", warmupErr)
		}
		log.WarnContext(ctx, "adapter.codex.ws_session.warmup_missing_response_id",
			"component", "adapter",
			"subcomponent", "codex",
			"conversation_id", conv,
		)
		return nil, errors.New("codex websocket warmup failed: missing response_id")
	}
	now := codexClock.Now()
	session := &WebsocketSession{
		Conn:           conn,
		ConversationID: conv,
		Model:          payload.Model,
		PromptCacheKey: payload.PromptCacheKey,
		LastResponseID: warmupResult.ResponseID,
		OpenedAt:       now,
		LastUsed:       now,
	}
	if log != nil {
		log.InfoContext(ctx, "adapter.codex.ws_session.opened",
			"component", "adapter",
			"subcomponent", "codex",
			"conversation_id", conv,
			"warmup_response_id", warmupResult.ResponseID,
		)
	}
	return session, nil
}

func websocketSessionCompatible(session *WebsocketSession, payload ResponseCreateWsRequest) bool {
	if session == nil {
		return false
	}
	sessionModel := strings.TrimSpace(session.Model)
	requestModel := strings.TrimSpace(payload.Model)
	if sessionModel != "" && requestModel != "" && sessionModel != requestModel {
		return false
	}
	sessionPromptCacheKey := strings.TrimSpace(session.PromptCacheKey)
	requestPromptCacheKey := strings.TrimSpace(payload.PromptCacheKey)
	if sessionPromptCacheKey != "" && requestPromptCacheKey != "" && sessionPromptCacheKey != requestPromptCacheKey {
		return false
	}
	return true
}
