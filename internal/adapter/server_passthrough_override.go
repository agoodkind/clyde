package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

const structuredOutputPassthroughOverrideParseFailedEvent = "passthrough_override structured-output parse failed; retrying"

type passthroughForwardOptions struct {
	requestID           string
	endpointPath        string
	baseURL             string
	apiKey              string
	upstreamLabel       string
	body                []byte
	contentEncoding     string
	streamRequested     bool
	streamIncrementally bool
	preserveCorrelation bool
	rawChatRequest      passthroughOverrideRequest
	jsonSpec            JSONResponseSpec
}

func (s *Server) forwardPassthroughOverride(
	w http.ResponseWriter,
	r *http.Request,
	req *adapterresolver.ResolvedRequest,
	body []byte,
) {
	reqID := newRequestID()
	streamRequested := false
	baseURL, apiKey, modelOverride, upstreamLabel, targetErr := passthroughUpstreamTarget(req)
	if targetErr != nil {
		s.respondAdapterError(w, r, targetErr)
		return
	}

	rawReq, jsonSpec, body, streamRequested := mutatePassthroughOverrideRequestBody(
		body,
		modelOverride,
		req.ProviderEffort().String(),
		streamRequested,
	)
	s.forwardPassthroughHTTP(
		w,
		r,
		req,
		passthroughForwardOptions{
			requestID:           reqID,
			endpointPath:        "/chat/completions",
			baseURL:             baseURL,
			apiKey:              apiKey,
			upstreamLabel:       upstreamLabel,
			body:                body,
			contentEncoding:     "",
			streamRequested:     streamRequested,
			streamIncrementally: false,
			preserveCorrelation: false,
			rawChatRequest:      rawReq,
			jsonSpec:            jsonSpec,
		},
	)
}

// passthroughUpstreamTarget resolves the effective upstream base URL, API key,
// model override, and telemetry label for a passthrough-override resolved
// request. A named override snapshot wins over the inline OpenAI-compat
// passthrough config. It returns a typed adapter error when a named override
// has no base URL configured. Both the chat and responses forwards share it so
// the config resolution lives in one place.
func passthroughUpstreamTarget(
	req *adapterresolver.ResolvedRequest,
) (baseURL, apiKey, modelOverride, upstreamLabel string, aerr *adapterError) {
	baseURL = strings.TrimSpace(req.OpenAICompatPassthrough.BaseURL)
	apiKey = req.OpenAICompatPassthrough.APIKey
	apiKeyEnv := req.OpenAICompatPassthrough.APIKeyEnv
	modelOverride = req.OpenAICompatPassthrough.Model
	upstreamLabel = "openai_compat_passthrough"
	if req.PassthroughOverrideName != "" {
		override := req.PassthroughOverride
		if override.BaseURL == "" {
			targetError := newAdapterError(
				adapterErrorUpstreamUnavailable,
				"alias routes to passthrough override "+req.PassthroughOverrideName+" but no base URL is configured",
			)
			targetError.Provider = providerName(req, "")
			targetError.Backend = req.Provider.String()
			targetError.ModelAlias = resolvedRequestAlias(req)

			return "", "", "", "", targetError
		}

		baseURL = override.BaseURL
		apiKey = override.APIKey
		apiKeyEnv = override.APIKeyEnv
		modelOverride = override.Model
		upstreamLabel = "passthrough_override:" + req.PassthroughOverrideName
	}

	if apiKey == "" && apiKeyEnv != "" {
		apiKey = os.Getenv(apiKeyEnv)
	}

	return baseURL, apiKey, modelOverride, upstreamLabel, nil
}

// forwardPassthroughResponses forwards a POST /v1/responses request to the
// passthrough-override upstream's /responses endpoint. Passthrough is a raw
// HTTP forward: the raw Responses body goes to the matching endpoint and the
// upstream response is written back verbatim, so an OpenAI-compatible upstream
// that implements the Responses API serves it directly. The chat-specific body
// mutation and JSON coercion do not apply to a Responses body, so only the
// model override is rewritten.
func (s *Server) forwardPassthroughResponses(
	w http.ResponseWriter,
	r *http.Request,
	reqID string,
	req *adapterresolver.ResolvedRequest,
	body []byte,
) {
	s.forwardPassthroughResponsesWire(w, r, reqID, req, body, body)
}

func (s *Server) forwardPassthroughHTTP(
	w http.ResponseWriter,
	r *http.Request,
	req *adapterresolver.ResolvedRequest,
	options passthroughForwardOptions,
) {
	started := clock.Now()
	corr := correlation.FromContext(r.Context())
	if !options.preserveCorrelation {
		corr = corr.Child()
	}

	corr = corr.WithRequestID(options.requestID)
	if corr.TraceID == "" {
		corr = clydeingress.FromHTTPHeader(r.Header, options.requestID)
	}

	clydeingress.SetHTTPHeaders(corr, w.Header())
	ctx := newPassthroughContext(r.Context(), corr)

	r = r.WithContext(ctx)
	alias := resolvedRequestAlias(req)
	s.emitRequestStarted(ctx, req, "", options.requestID, alias, options.streamRequested)

	resp, err := s.openPassthroughResponse(ctx, options, started)
	if err != nil {
		s.respondPassthroughOverrideTransportError(
			w,
			r,
			ctx,
			req,
			options.requestID,
			started,
			options.streamRequested,
			err,
		)

		return
	}

	defer func() { _ = resp.Body.Close() }()
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if resp.StatusCode < http.StatusOK {
		s.respondPassthroughInformational(ctx, w, r, req, &options, resp, started, contentType)

		return
	}

	if resp.StatusCode >= http.StatusMultipleChoices {
		s.respondPassthroughRejectedResponse(w, r, ctx, req, resp, options, started, contentType)
		return
	}

	streamOpened := options.streamRequested || strings.Contains(contentType, "text/event-stream")
	if streamOpened {
		s.emitRequestStreamOpened(ctx, req, "", options.requestID, alias)
	}

	if options.streamIncrementally && streamOpened {
		s.forwardPassthroughStream(ctx, w, req, &options, resp, started, contentType)

		return
	}

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		s.recordPassthroughEgress(
			ctx,
			resp,
			options.body,
			passthroughCaptureResultFromRead(respBody, readErr),
			started,
		)
		s.respondPassthroughOverrideTransportError(
			w,
			r,
			ctx,
			req,
			options.requestID,
			started,
			options.streamRequested,
			readErr,
		)

		return
	}

	s.finishPassthroughResponse(
		ctx,
		corr,
		w,
		r,
		req,
		&options,
		resp,
		&respBody,
		started,
		contentType,
	)
}

func (s *Server) openPassthroughResponse(
	ctx context.Context,
	options passthroughForwardOptions,
	started time.Time,
) (*http.Response, error) {
	request, err := newPassthroughOverrideRequest(
		ctx,
		options.baseURL,
		options.apiKey,
		options.body,
		options.endpointPath,
	)
	if err != nil {
		return nil, err
	}

	if options.contentEncoding != "" {
		request.Header.Set("Content-Encoding", options.contentEncoding)
	}

	response, err := passthroughOverrideDoRequest(request)
	if err != nil {
		s.recordPassthroughEgressAttempt(
			ctx,
			request,
			nil,
			options.body,
			passthroughCaptureResultFromBody(nil),
			started,
		)

		return nil, err
	}

	return response, nil
}

type passthroughCaptureResult struct {
	body       []byte
	totalBytes int
	truncated  bool
	usage      Usage
}

func (s *Server) copyPassthroughResponse(
	ctx context.Context,
	w http.ResponseWriter,
	resp *http.Response,
	flush bool,
) (passthroughCaptureResult, error) {
	copyPassthroughHeaders(w.Header(), resp.Header)
	clydeingress.SetHTTPHeaders(correlation.FromContext(ctx), w.Header())
	w.WriteHeader(resp.StatusCode)
	retainedBytes := 0
	if s.deps.CaptureStore.RecordingEnabled() {
		retainedBytes = capture.DefaultMaxBodyBytes
	}

	captured := capture.NewCappedBuffer(retainedBytes)
	usageParser := newPassthroughSSEUsageParser()
	buffer := make([]byte, 32*1024)

	for {
		count, readErr := resp.Body.Read(buffer)
		if count > 0 {
			writeErr := s.writePassthroughChunk(
				ctx,
				w,
				buffer[:count],
				captured,
				usageParser,
				flush,
			)
			if writeErr != nil {
				return passthroughStreamCaptureResult(captured, usageParser, true), writeErr
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				return passthroughStreamCaptureResult(captured, usageParser, false), nil
			}

			s.log.WarnContext(
				ctx,
				"adapter.passthrough_override.read_response_failed",
				"concern",
				"adapter.providers.passthrough_override.response",
				"err",
				readErr,
			)

			return passthroughStreamCaptureResult(captured, usageParser, true), fmt.Errorf(
				"read passthrough response: %w",
				readErr,
			)
		}
	}
}

func (s *Server) writePassthroughChunk(
	ctx context.Context,
	w http.ResponseWriter,
	chunk []byte,
	captured *capture.CappedBuffer,
	usageParser *passthroughSSEUsageParser,
	flush bool,
) error {
	_, _ = captured.Write(chunk)
	usageParser.Write(chunk)
	if _, err := w.Write(chunk); err != nil {
		s.log.WarnContext(
			ctx,
			"adapter.passthrough_override.write_response_failed",
			"concern",
			"adapter.providers.passthrough_override.response",
			"err",
			err,
		)

		return fmt.Errorf("write passthrough response: %w", err)
	}

	if flush {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}

	return nil
}

type passthroughFramingHeader string

const (
	passthroughHeaderConnection         passthroughFramingHeader = "Connection"
	passthroughHeaderKeepAlive          passthroughFramingHeader = "Keep-Alive"
	passthroughHeaderProxyAuthenticate  passthroughFramingHeader = "Proxy-Authenticate"
	passthroughHeaderProxyAuthorization passthroughFramingHeader = "Proxy-" + "Authorization"
	passthroughHeaderTE                 passthroughFramingHeader = "Te"
	passthroughHeaderTrailer            passthroughFramingHeader = "Trailer"
	passthroughHeaderTransferEncoding   passthroughFramingHeader = "Transfer-" + "Encoding"
	passthroughHeaderUpgrade            passthroughFramingHeader = "Upgrade"
	passthroughHeaderContentLength      passthroughFramingHeader = "Content-Length"
	passthroughHeaderProxyConnection    passthroughFramingHeader = "Proxy-" + "Connection"
)

// passthroughBodyStreamRequested reports whether a request body set stream:true.
func passthroughBodyStreamRequested(body []byte) bool {
	var fields struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return false
	}

	return fields.Stream
}

// passthroughOverrideRequest carries the inbound OpenAI Chat
// Completions request body as a map of named raw JSON fields. Each
// field stays opaque until a specific accessor decodes it; that lets
// the passthrough surface mutate the body without losing fields the
// upstream cares about.
type passthroughOverrideRequest map[string]json.RawMessage

// passthroughOverrideMessage is the typed view of one entry in the
// `messages` array used by the chat-completions request body. The
// system-message injection helpers operate on this shape.
type passthroughOverrideMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
}

// respondPassthroughOverrideTransportError handles the transport-level
// failure case where the upstream call never produced a status. The
// envelope is shaped through the boundary so Cursor BYOK still
// renders the upstream connection error rather than a fallback
// message.
func (s *Server) respondPassthroughOverrideTransportError(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	req *adapterresolver.ResolvedRequest,
	reqID string,
	started time.Time,
	streamRequested bool,
	err error,
) {
	alias := resolvedRequestAlias(req)
	adapterruntime.LogTerminal(
		s.log,
		ctx,
		s.deps.RequestEvents,
		adapterruntime.RequestEvent{
			Stage:                      adapterruntime.RequestStageFailed,
			Provider:                   providerName(req, ""),
			Backend:                    req.Provider.String(),
			RequestID:                  reqID,
			ExecutionID:                adapterruntime.ExecutionIDFromContext(ctx),
			Alias:                      alias,
			ModelID:                    alias,
			Stream:                     streamRequested,
			DurationMs:                 clock.Since(started).Milliseconds(),
			Err:                        err.Error(),
			FinishReason:               "",
			TokensIn:                   0,
			TokensOut:                  0,
			CacheReadTokens:            0,
			CacheCreationTokens:        0,
			DerivedCacheCreationTokens: 0,
			ToolCallCount:              0,
			ToolCallNames:              nil,
			HasSubagentToolCall:        false,
			Correlation: correlation.
				Context{
				TraceID:            "",
				SpanID:             "",
				ParentSpanID:       "",
				RequestID:          "",
				IdentityAttributes: nil,
			},
		},
	)
	aerr := newAdapterError(adapterErrorUpstreamUnavailable, err.Error())
	aerr.Provider = providerName(req, "")
	aerr.Backend = req.Provider.String()
	aerr.ModelAlias = alias
	aerr.Cause = err
	s.respondAdapterError(w, r, aerr)
}

// writePassthroughOverrideResponse forwards a 2xx upstream body to the
// caller verbatim. The caller flow has already early-returned on
// non-2xx so this writer only ever sees a status that is already in
// the OpenAI compat success window.
func writePassthroughOverrideResponse(
	corr correlation.Context,
	w http.ResponseWriter,
	status int,
	respBody []byte,
	hdr http.Header,
) {
	copyPassthroughHeaders(w.Header(), hdr)
	clydeingress.SetHTTPHeaders(corr, w.Header())
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

func (s *Server) respondPassthroughStreamCopyError(
	ctx context.Context,
	w http.ResponseWriter,
	req *adapterresolver.ResolvedRequest,
	reqID string,
	started time.Time,
	err error,
) error {
	s.logPassthroughOverrideFailure(ctx, req, reqID, started, true, err)
	aerr := mapUpstreamForFamily(
		adapterRouteOpenAI,
		providerName(req, ""),
		http.StatusBadGateway,
		upstreamClassNetworkError,
		"",
		err.Error(),
	)
	aerr.Backend = req.Provider.String()
	aerr.ModelAlias = resolvedRequestAlias(req)
	aerr.Cause = err
	sse, sseErr := adapteropenai.NewSSEWriter(w)
	if sseErr != nil {
		s.log.WarnContext(
			ctx,
			"adapter.passthrough_override.create_stream_error_writer_failed",
			"concern",
			"adapter.providers.passthrough_override.response",
			"err",
			sseErr,
		)

		return fmt.Errorf("create passthrough stream error writer: %w", sseErr)
	}

	if boundaryErr := s.respondAdapterStreamError(ctx, sse, aerr); boundaryErr != nil {
		s.log.WarnContext(
			ctx,
			"adapter.passthrough_override.respond_stream_error_failed",
			"concern",
			"adapter.providers.passthrough_override.response",
			"err",
			boundaryErr,
		)

		return fmt.Errorf("respond passthrough stream error: %w", boundaryErr)
	}

	return nil
}

// coerceOrRetryPassthroughOverrideJSON folds the JSON-spec coercion
// and one-shot retry path into a helper so forwardPassthroughOverride
// stays under the funlen budget. Returns the (possibly rewritten)
// body, status, and headers.
func (s *Server) coerceOrRetryPassthroughOverrideJSON(
	ctx context.Context,
	corr correlation.Context,
	req *adapterresolver.ResolvedRequest,
	upstreamLabel string,
	baseURL string,
	apiKey string,
	rawReq passthroughOverrideRequest,
	jsonSpec JSONResponseSpec,
	firstResponse *http.Response,
	firstRequestBody []byte,
	respBody []byte,
	status int,
	hdr http.Header,
	started time.Time,
) ([]byte, int, http.Header, bool, *passthroughRetryFailure) {
	coerced, ok := coercePassthroughOverrideJSON(respBody)
	if ok {
		return coerced, status, hdr, false, nil
	}

	attrs := []slog.Attr{
		slog.String("model", resolvedRequestAlias(req)),
		slog.String("upstream", upstreamLabel),
		slog.Int("first_attempt_bytes", len(respBody)),
	}
	attrs = append(attrs, corr.Attrs()...)
	s.logPassthroughJSONRetry(ctx, attrs)
	injectJSONSystemMessage(rawReq, jsonSpec.SystemPrompt(true))
	body2, err := json.Marshal(rawReq)
	if err != nil {
		return respBody, status, hdr, false, nil
	}

	s.recordPassthroughEgress(
		ctx,
		firstResponse,
		firstRequestBody,
		passthroughCaptureResultFromBody(respBody),
		started,
	)
	secondStarted := clock.Now()
	secondRequest, requestErr := newPassthroughOverrideRequest(
		ctx,
		baseURL,
		apiKey,
		body2,
		"/chat/completions",
	)
	if requestErr != nil {
		return respBody, status, hdr, true, newPassthroughRetryFailure(0, nil, "", requestErr)
	}

	secondResponse, err2 := passthroughOverrideDoRequest(secondRequest)
	if err2 != nil {
		s.recordPassthroughEgressAttempt(
			ctx,
			secondRequest,
			nil,
			body2,
			passthroughCaptureResultFromBody(nil),
			secondStarted,
		)

		return respBody, status, hdr, true, newPassthroughRetryFailure(0, nil, "", err2)
	}

	defer func() { _ = secondResponse.Body.Close() }()
	rb2, readErr := io.ReadAll(secondResponse.Body)
	s.recordPassthroughEgress(
		ctx,
		secondResponse,
		body2,
		passthroughCaptureResultFromRead(rb2, readErr),
		secondStarted,
	)
	if readErr != nil {
		return respBody, status, hdr, true, newPassthroughRetryFailure(
			secondResponse.StatusCode,
			rb2,
			secondResponse.Header.Get("Content-Type"),
			readErr,
		)
	}

	st2 := secondResponse.StatusCode
	h2 := secondResponse.Header
	if st2 != http.StatusOK {
		return rb2, st2, h2, true, newPassthroughRetryFailure(st2, rb2, h2.Get("Content-Type"), nil)
	}

	if c2, ok2 := coercePassthroughOverrideJSON(rb2); ok2 {
		return c2, st2, h2, true, nil
	}

	return rb2, st2, h2, true, nil
}

// respondPassthroughOverrideError writes a Cursor-safe envelope for a
// passthrough override upstream that returned a 4xx or 5xx, and emits
// the matching terminal log event. Extracted from
// forwardPassthroughOverride so the parent function stays under the
// funlen budget while keeping the error path explicit.
func (s *Server) respondPassthroughOverrideError(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	req *adapterresolver.ResolvedRequest,
	reqID string,
	status int,
	respBody []byte,
	streamRequested bool,
	contentType string,
	started time.Time,
) {
	alias := resolvedRequestAlias(req)
	message := passthroughOverrideUpstreamErrorMessage(status, respBody)
	codeClass := anthropicCodeClassForStatus(status)
	aerr := mapUpstreamForFamily(
		adapterRouteOpenAI,
		providerName(req, ""),
		status,
		codeClass,
		"",
		message,
	)
	aerr.Backend = req.Provider.String()
	aerr.ModelAlias = alias
	s.writeShapedError(w, r, aerr)
	adapterruntime.LogTerminal(
		s.log,
		ctx,
		s.deps.RequestEvents,
		adapterruntime.RequestEvent{
			Stage:       adapterruntime.RequestStageFailed,
			Provider:    providerName(req, ""),
			Backend:     req.Provider.String(),
			RequestID:   reqID,
			ExecutionID: adapterruntime.ExecutionIDFromContext(ctx),
			Alias:       alias,
			ModelID:     alias,
			Stream: streamRequested ||
				strings.Contains(contentType, "text/event-stream"),
			DurationMs:                 clock.Since(started).Milliseconds(),
			Err:                        "upstream returned status " + strconv.Itoa(status),
			FinishReason:               "",
			TokensIn:                   0,
			TokensOut:                  0,
			CacheReadTokens:            0,
			CacheCreationTokens:        0,
			DerivedCacheCreationTokens: 0,
			ToolCallCount:              0,
			ToolCallNames:              nil,
			HasSubagentToolCall:        false,
			Correlation: correlation.
				Context{
				TraceID:            "",
				SpanID:             "",
				ParentSpanID:       "",
				RequestID:          "",
				IdentityAttributes: nil,
			},
		},
	)
}

// injectJSONSystemMessage prepends (or appends to existing system
// content) an instruction telling the model to emit raw JSON only.
func injectJSONSystemMessage(req passthroughOverrideRequest, instruction string) {
	if instruction == "" {
		return
	}

	rawMessages := req["messages"]
	var msgs []passthroughOverrideMessage
	if len(rawMessages) > 0 {
		if err := json.Unmarshal(rawMessages, &msgs); err != nil {
			msgs = nil
		}
	}

	if injectJSONSystemInstructionIntoFirstMessage(msgs, instruction) {
		encoded, err := json.Marshal(msgs)
		if err == nil {
			req["messages"] = encoded
		}

		return
	}

	sys := passthroughOverrideMessage{
		Role:    "system",
		Content: jsonEncodedString(instruction),
	}
	msgs = append([]passthroughOverrideMessage{sys}, msgs...)
	encoded, err := json.Marshal(msgs)
	if err == nil {
		req["messages"] = encoded
	}
}

func newPassthroughOverrideRequest(
	ctx context.Context,
	baseURL string,
	apiKey string,
	body []byte,
	endpointPath string,
) (*http.Request, error) {
	target, targetErr := passthroughOverrideTarget(ctx, baseURL, endpointPath)
	if targetErr != nil {
		return nil, targetErr
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		target,
		strings.NewReader(string(body)),
	)
	if err != nil {
		safeTarget := sanitizedPassthroughTarget(target)
		sanitizedErr := fmt.Errorf("create passthrough override request for %s failed", safeTarget)
		slog.WarnContext(
			ctx,
			"adapter.passthrough_override.create_request_failed",
			"concern",
			"adapter.providers.passthrough_override.request",
			"target",
			safeTarget,
			"err",
			sanitizedErr,
		)

		return nil, sanitizedErr
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	return req, nil
}

func passthroughOverrideDoRequest(req *http.Request) (*http.Response, error) {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		safeTarget := sanitizedPassthroughTarget(req.URL.String())
		sanitizedErr := fmt.Errorf("post passthrough override request to %s failed", safeTarget)
		slog.WarnContext(
			req.Context(),
			"adapter.passthrough_override.post_request_failed",
			"concern",
			"adapter.providers.passthrough_override.request",
			"target",
			safeTarget,
			"err",
			sanitizedErr,
		)

		return nil, sanitizedErr
	}

	return resp, nil
}

func passthroughOverrideTarget(
	ctx context.Context,
	baseURL string,
	endpointPath string,
) (string, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		safeTarget := sanitizedPassthroughTarget(baseURL)
		sanitizedErr := fmt.Errorf("parse passthrough target %s failed", safeTarget)
		slog.WarnContext(
			ctx,
			"adapter.passthrough_override.parse_target_failed",
			"concern",
			"adapter.providers.passthrough_override.request",
			"target",
			safeTarget,
			"err",
			sanitizedErr,
		)

		return "", sanitizedErr
	}

	target.Path = strings.TrimRight(target.Path, "/") + endpointPath
	target.RawPath = ""

	return target.String(), nil
}

// coercePassthroughOverrideJSON walks the OpenAI-shaped response, runs CoerceJSON
// on choices[].message.content, and returns the rewritten body if all
// choices now parse as JSON.
func coercePassthroughOverrideJSON(body []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, false
	}

	rawChoices, ok := fields["choices"]
	if !ok {
		return body, false
	}

	var choices []json.RawMessage
	if err := json.Unmarshal(rawChoices, &choices); err != nil || len(choices) == 0 {
		return body, false
	}

	allOK := true

	for choiceIndex, raw := range choices {
		coerced, ok := coercePassthroughOverrideChoice(raw)
		if !ok {
			allOK = false
		}

		choices[choiceIndex] = coerced
	}

	rawChoices, err := json.Marshal(choices)
	if err != nil {
		return body, false
	}

	fields["choices"] = rawChoices
	out, err := json.Marshal(fields)
	if err != nil {
		return body, false
	}

	return out, allOK
}

// coercePassthroughOverrideChoice rewrites the choice's message
// content via [CoerceJSON]. Returns the new raw bytes and a flag
// indicating whether the coerced content parses as JSON. When the
// shape is unrecognized, the original raw is returned.
func coercePassthroughOverrideChoice(raw json.RawMessage) (json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw, false
	}

	rawMessage, ok := fields["message"]
	if !ok {
		return raw, true
	}

	var msgFields map[string]json.RawMessage
	if err := json.Unmarshal(rawMessage, &msgFields); err != nil {
		return raw, true
	}

	rawContent, ok := msgFields["content"]
	if !ok {
		return raw, true
	}

	var content string
	if err := json.Unmarshal(rawContent, &content); err != nil || content == "" {
		return raw, true
	}

	coerced := CoerceJSON(content)
	if !LooksLikeJSON(coerced) {
		return raw, false
	}

	encoded, err := json.Marshal(coerced)
	if err != nil {
		return raw, false
	}

	msgFields["content"] = encoded
	rebuiltMessage, err := json.Marshal(msgFields)
	if err != nil {
		return raw, false
	}

	fields["message"] = rebuiltMessage
	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return raw, false
	}

	return rebuilt, true
}

func redactedHeaders(input http.Header) map[string]string {
	out := make(map[string]string, len(input))
	for key, values := range input {
		normalized := strings.ToLower(key)
		if redactedHeader(normalized) {
			out[normalized] = "[redacted]"
			continue
		}

		out[normalized] = strings.Join(values, ", ")
	}

	return out
}

// passthroughRedactedHeader enumerates the request header names the
// passthrough override surface scrubs to "[REDACTED]" before
// recording the inbound request in capture logs.
type passthroughRedactedHeader string

const (
	passthroughRedactAuthorization      passthroughRedactedHeader = "authorization"
	passthroughRedactProxyAuthorization passthroughRedactedHeader = "proxy-authorization"
	passthroughRedactCookie             passthroughRedactedHeader = "cookie"
	passthroughRedactSetCookie          passthroughRedactedHeader = "set-cookie"
	passthroughRedactClydeHeader        passthroughRedactedHeader = "x-clyde-" + "token"
	passthroughRedactAWSSecurityHeader  passthroughRedactedHeader = "x-amz-security-" + "token"
	passthroughRedactOpenAIIdentHeader  passthroughRedactedHeader = "openai-api-" + "key"
	passthroughRedactEmpty              passthroughRedactedHeader = ""
)

func injectJSONSystemInstructionIntoFirstMessage(
	msgs []passthroughOverrideMessage,
	instruction string,
) bool {
	if len(msgs) == 0 {
		return false
	}

	first := msgs[0]
	if first.Role != "system" && first.Role != "developer" {
		return false
	}

	var existing string
	if len(first.Content) > 0 {
		if err := json.Unmarshal(first.Content, &existing); err == nil && existing != "" {
			first.Content = jsonEncodedString(instruction + "\n\n" + existing)
		} else {
			first.Content = jsonEncodedString(instruction)
		}
	} else {
		first.Content = jsonEncodedString(instruction)
	}

	msgs[0] = first

	return true
}
