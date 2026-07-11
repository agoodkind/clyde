package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	"goodkind.io/clyde/internal/slogger"
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
	streamRequested     bool
	streamIncrementally bool
	preserveCorrelation bool
	rawChatRequest      passthroughOverrideRequest
	jsonSpec            JSONResponseSpec
}

func (s *Server) forwardPassthroughOverride(w http.ResponseWriter, r *http.Request, req *adapterresolver.ResolvedRequest, body []byte) {
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
	s.forwardPassthroughHTTP(w, r, req, passthroughForwardOptions{
		requestID: reqID, endpointPath: "/chat/completions", baseURL: baseURL, apiKey: apiKey,
		upstreamLabel: upstreamLabel, body: body, streamRequested: streamRequested,
		streamIncrementally: false, preserveCorrelation: false, rawChatRequest: rawReq, jsonSpec: jsonSpec,
	})
}

// passthroughUpstreamTarget resolves the effective upstream base URL, API key,
// model override, and telemetry label for a passthrough-override resolved
// request. A named override snapshot wins over the inline OpenAI-compat
// passthrough config. It returns a typed adapter error when a named override
// has no base URL configured. Both the chat and responses forwards share it so
// the config resolution lives in one place.
func passthroughUpstreamTarget(req *adapterresolver.ResolvedRequest) (baseURL, apiKey, modelOverride, upstreamLabel string, aerr *adapterError) {
	baseURL = strings.TrimSpace(req.OpenAICompatPassthrough.BaseURL)
	apiKey = req.OpenAICompatPassthrough.APIKey
	apiKeyEnv := req.OpenAICompatPassthrough.APIKeyEnv
	modelOverride = req.OpenAICompatPassthrough.Model
	upstreamLabel = "openai_compat_passthrough"
	if req.PassthroughOverrideName != "" {
		override := req.PassthroughOverride
		if override.BaseURL == "" {
			e := newAdapterError(adapterErrorUpstreamUnavailable,
				"alias routes to passthrough override "+req.PassthroughOverrideName+" but no base URL is configured")
			e.Provider = providerName(req, "")
			e.Backend = req.Provider.String()
			e.ModelAlias = resolvedRequestAlias(req)
			return "", "", "", "", e
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
func (s *Server) forwardPassthroughResponses(w http.ResponseWriter, r *http.Request, reqID string, req *adapterresolver.ResolvedRequest, body []byte) {
	baseURL, apiKey, modelOverride, upstreamLabel, targetErr := passthroughUpstreamTarget(req)
	if targetErr != nil {
		s.respondAdapterError(w, r, targetErr)
		return
	}
	body = passthroughResponsesBodyWithModel(body, modelOverride)
	streamRequested := passthroughBodyStreamRequested(body)
	s.forwardPassthroughHTTP(w, r, req, passthroughForwardOptions{
		requestID: reqID, endpointPath: "/responses", baseURL: baseURL, apiKey: apiKey,
		upstreamLabel: upstreamLabel, body: body, streamRequested: streamRequested,
		streamIncrementally: true, preserveCorrelation: true, rawChatRequest: nil,
		jsonSpec: JSONResponseSpec{Mode: "", SchemaName: "", Schema: nil},
	})
}

func (s *Server) forwardPassthroughHTTP(w http.ResponseWriter, r *http.Request, req *adapterresolver.ResolvedRequest, options passthroughForwardOptions) {
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
	ctx := correlation.WithContext(r.Context(), corr)
	r = r.WithContext(ctx)
	alias := resolvedRequestAlias(req)
	s.emitRequestStarted(ctx, req, "", options.requestID, alias, options.streamRequested)

	resp, err := passthroughOverrideDo(ctx, options.baseURL, options.apiKey, options.body, options.endpointPath)
	if err != nil {
		s.respondPassthroughOverrideTransportError(w, r, ctx, req, options.requestID, started, options.streamRequested, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	streamOpened := options.streamRequested || strings.Contains(contentType, "text/event-stream")
	if streamOpened {
		s.emitRequestStreamOpened(ctx, req, "", options.requestID, alias)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			s.respondPassthroughOverrideTransportError(w, r, ctx, req, options.requestID, started, options.streamRequested, readErr)
			return
		}
		s.recordPassthroughEgress(ctx, resp, options.body, passthroughCaptureResultFromBody(respBody), started)
		s.respondPassthroughOverrideError(w, r, ctx, req, options.requestID, resp.StatusCode, respBody, options.streamRequested, contentType, started)
		return
	}
	if options.streamIncrementally && streamOpened {
		copyResult, copyErr := s.copyPassthroughResponse(ctx, w, resp, true)
		s.recordPassthroughEgress(ctx, resp, options.body, copyResult, started)
		if copyErr != nil {
			s.log.WarnContext(ctx, "adapter.passthrough_override.copy_response_failed", "concern", "adapter.providers.passthrough_override.response", "err", copyErr)
			if boundaryErr := s.respondPassthroughStreamCopyError(ctx, w, req, options.requestID, started, copyErr); boundaryErr != nil {
				s.log.WarnContext(ctx, "adapter.passthrough_override.stream_error_boundary_failed", "concern", "adapter.providers.passthrough_override.response", "err", boundaryErr)
			}
			return
		}
		s.logPassthroughOverrideTerminal(ctx, req, options.requestID, started, options.streamRequested, contentType, copyResult.usage)
		return
	}
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		s.respondPassthroughOverrideTransportError(w, r, ctx, req, options.requestID, started, options.streamRequested, readErr)
		return
	}
	status := resp.StatusCode
	header := resp.Header
	if options.jsonSpec.Mode != "" && status == http.StatusOK {
		var captureRecorded bool
		respBody, status, header, captureRecorded = s.coerceOrRetryPassthroughOverrideJSON(
			ctx, corr, req, options.upstreamLabel, options.baseURL, options.apiKey,
			options.rawChatRequest, options.jsonSpec, resp, options.body, respBody, status, header, started,
		)
		if !captureRecorded {
			s.recordPassthroughEgress(ctx, resp, options.body, passthroughCaptureResultFromBody(respBody), started)
		}
	} else {
		s.recordPassthroughEgress(ctx, resp, options.body, passthroughCaptureResultFromBody(respBody), started)
	}
	writePassthroughOverrideResponse(w, status, respBody, header)
	s.logPassthroughOverrideTerminal(ctx, req, options.requestID, started, options.streamRequested, contentType, passthroughOverrideUsageFromBody(respBody))
}

type passthroughCaptureResult struct {
	body       []byte
	totalBytes int
	truncated  bool
	usage      Usage
}

func passthroughCaptureResultFromBody(body []byte) passthroughCaptureResult {
	return passthroughCaptureResult{body: body, totalBytes: len(body), truncated: false, usage: passthroughOverrideUsageFromBody(body)}
}

func (s *Server) copyPassthroughResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, flush bool) (passthroughCaptureResult, error) {
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	captured := capture.NewCappedBuffer(capture.DefaultMaxBodyBytes)
	usageParser := newPassthroughSSEUsageParser()
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := resp.Body.Read(buffer)
		if count > 0 {
			writeErr := s.writePassthroughChunk(ctx, w, buffer[:count], captured, usageParser, flush)
			if writeErr != nil {
				return passthroughStreamCaptureResult(captured, usageParser), writeErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return passthroughStreamCaptureResult(captured, usageParser), nil
			}
			s.log.WarnContext(ctx, "adapter.passthrough_override.read_response_failed", "concern", "adapter.providers.passthrough_override.response", "err", readErr)
			return passthroughStreamCaptureResult(captured, usageParser), fmt.Errorf("read passthrough response: %w", readErr)
		}
	}
}

func (s *Server) writePassthroughChunk(ctx context.Context, w http.ResponseWriter, chunk []byte, captured *capture.CappedBuffer, usageParser *passthroughSSEUsageParser, flush bool) error {
	_, _ = captured.Write(chunk)
	usageParser.Write(chunk)
	if _, err := w.Write(chunk); err != nil {
		s.log.WarnContext(ctx, "adapter.passthrough_override.write_response_failed", "concern", "adapter.providers.passthrough_override.response", "err", err)
		return fmt.Errorf("write passthrough response: %w", err)
	}
	if flush {
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return nil
}

func passthroughStreamCaptureResult(captured *capture.CappedBuffer, usageParser *passthroughSSEUsageParser) passthroughCaptureResult {
	return passthroughCaptureResult{
		body: captured.Bytes(), totalBytes: captured.TotalRead(), truncated: captured.Truncated(),
		usage: usageParser.Usage(),
	}
}

func copyPassthroughHeaders(dst, src http.Header) {
	for key, values := range src {
		switch passthroughFramingHeader(http.CanonicalHeaderKey(key)) {
		case passthroughHeaderConnection, passthroughHeaderKeepAlive, passthroughHeaderProxyAuthenticate,
			passthroughHeaderProxyAuthorization, passthroughHeaderTE, passthroughHeaderTrailer,
			passthroughHeaderTransferEncoding, passthroughHeaderUpgrade, passthroughHeaderContentLength:
			continue
		default:
		}
		dst[key] = values
	}
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
)

func (s *Server) recordPassthroughEgress(ctx context.Context, resp *http.Response, requestBody []byte, result passthroughCaptureResult, started time.Time) {
	if s.deps.CaptureStore == nil || resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return
	}
	requestHeaders := resp.Request.Header.Clone()
	for name := range requestHeaders {
		if redactedHeader(strings.ToLower(name)) {
			requestHeaders.Del(name)
		}
	}
	responseHeaders := sanitizedCaptureResponseHeaders(resp.Header)
	s.deps.CaptureStore.RecordCappedExchange(correlation.FromContext(ctx), capture.Exchange{
		Client:            "adapter.passthrough",
		Provider:          "openai-compatible",
		Concern:           "adapter.passthrough.egress",
		Host:              resp.Request.URL.Host,
		Method:            resp.Request.Method,
		Path:              resp.Request.URL.Path,
		Status:            resp.StatusCode,
		UpstreamRequestID: resp.Header.Get("Request-Id"),
		SessionID:         "",
		RequestHeaders:    requestHeaders,
		ResponseHeaders:   responseHeaders,
		RequestBody:       requestBody,
		ResponseBody:      result.body,
		RequestType:       resp.Request.Header.Get("Content-Type"),
		ResponseType:      resp.Header.Get("Content-Type"),
		Started:           started,
	}, result.totalBytes, result.truncated)
}

// passthroughResponsesBodyWithModel rewrites the "model" field of a Responses
// request body to the configured wire model when an override is set, leaving
// every other field untouched. A non-object or unparseable body is forwarded
// unchanged.
func passthroughResponsesBodyWithModel(body []byte, modelOverride string) []byte {
	if modelOverride == "" {
		return body
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body
	}
	encoded, err := json.Marshal(modelOverride)
	if err != nil {
		return body
	}
	fields["model"] = encoded
	rewritten, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return rewritten
}

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

// mutatePassthroughOverrideRequestBody applies the alias rewrite and
// json-spec extraction in one place so forwardPassthroughOverride
// stays under the funlen budget. Returns the parsed request (or
// nil), the parsed json spec, the (possibly rewritten) request
// bytes, and whether the request asked for a streaming response.
func mutatePassthroughOverrideRequestBody(
	body []byte,
	modelOverride string,
	wireEffort string,
	streamRequested bool,
) (passthroughOverrideRequest, JSONResponseSpec, []byte, bool) {
	rawReq := passthroughOverrideRequest{}
	jsonSpec := JSONResponseSpec{Mode: "", SchemaName: "", Schema: nil}
	if err := json.Unmarshal(body, &rawReq); err != nil {
		return nil, jsonSpec, body, streamRequested
	}
	if v, ok := rawReq["stream"]; ok {
		var streamVal bool
		if err := json.Unmarshal(v, &streamVal); err == nil {
			streamRequested = streamVal
		}
	}
	if modelOverride != "" {
		encoded, err := json.Marshal(modelOverride)
		if err == nil {
			rawReq["model"] = encoded
		}
	}
	applyPassthroughWireEffort(rawReq, wireEffort)
	if rf, ok := rawReq["response_format"]; ok {
		jsonSpec = ParseResponseFormat(rf)
	}
	if jsonSpec.Mode != "" {
		injectJSONSystemMessage(rawReq, jsonSpec.SystemPrompt(false))
		delete(rawReq, "response_format")
	}
	rewritten, err := json.Marshal(rawReq)
	if err != nil {
		return rawReq, jsonSpec, body, streamRequested
	}
	return rawReq, jsonSpec, rewritten, streamRequested
}

func applyPassthroughWireEffort(rawReq passthroughOverrideRequest, wireEffort string) {
	if wireEffort == "" {
		return
	}
	encodedEffort, err := json.Marshal(wireEffort)
	if err != nil {
		return
	}
	rewroteField := false
	if _, ok := rawReq["reasoning_effort"]; ok {
		rawReq["reasoning_effort"] = encodedEffort
		rewroteField = true
	}
	if rawReasoning, ok := rawReq["reasoning"]; ok {
		if encodedReasoning, ok := passthroughReasoningWithWireEffort(rawReasoning, encodedEffort); ok {
			rawReq["reasoning"] = encodedReasoning
			rewroteField = true
		}
	}
	if !rewroteField {
		rawReq["reasoning_effort"] = encodedEffort
	}
}

func passthroughReasoningWithWireEffort(
	rawReasoning json.RawMessage,
	encodedEffort json.RawMessage,
) (json.RawMessage, bool) {
	var reasoning map[string]json.RawMessage
	if err := json.Unmarshal(rawReasoning, &reasoning); err != nil {
		return nil, false
	}
	if reasoning == nil {
		reasoning = make(map[string]json.RawMessage)
	}
	reasoning["effort"] = encodedEffort
	encodedReasoning, err := json.Marshal(reasoning)
	if err != nil {
		return nil, false
	}
	return encodedReasoning, true
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
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:      adapterruntime.RequestStageFailed,
		Provider:   providerName(req, ""),
		Backend:    req.Provider.String(),
		RequestID:  reqID,
		Alias:      alias,
		ModelID:    alias,
		Stream:     streamRequested,
		DurationMs: clock.Since(started).Milliseconds(),
		Err:        err.Error(), FinishReason: "", TokensIn: 0, TokensOut: 0, CacheReadTokens: 0, CacheCreationTokens: 0, DerivedCacheCreationTokens: 0, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false, Correlation: correlation.
				Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	})
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
func writePassthroughOverrideResponse(w http.ResponseWriter, status int, respBody []byte, hdr http.Header) {
	copyPassthroughHeaders(w.Header(), hdr)
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

func (s *Server) logPassthroughOverrideFailure(ctx context.Context, req *adapterresolver.ResolvedRequest, reqID string, started time.Time, stream bool, err error) {
	alias := resolvedRequestAlias(req)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage: adapterruntime.RequestStageFailed, Provider: providerName(req, ""), Backend: req.Provider.String(),
		RequestID: reqID, Alias: alias, ModelID: alias, Stream: stream,
		DurationMs: clock.Since(started).Milliseconds(), Err: err.Error(), FinishReason: "", TokensIn: 0,
		TokensOut: 0, CacheReadTokens: 0, CacheCreationTokens: 0, DerivedCacheCreationTokens: 0,
		ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false, Correlation: correlation.Context{},
	})
}

func (s *Server) respondPassthroughStreamCopyError(ctx context.Context, w http.ResponseWriter, req *adapterresolver.ResolvedRequest, reqID string, started time.Time, err error) error {
	s.logPassthroughOverrideFailure(ctx, req, reqID, started, true, err)
	aerr := mapUpstreamForFamily(adapterRouteOpenAI, providerName(req, ""), http.StatusBadGateway, upstreamClassNetworkError, "", err.Error())
	aerr.Backend = req.Provider.String()
	aerr.ModelAlias = resolvedRequestAlias(req)
	aerr.Cause = err
	sse, sseErr := adapteropenai.NewSSEWriter(w)
	if sseErr != nil {
		s.log.WarnContext(ctx, "adapter.passthrough_override.create_stream_error_writer_failed", "concern", "adapter.providers.passthrough_override.response", "err", sseErr)
		return fmt.Errorf("create passthrough stream error writer: %w", sseErr)
	}
	if boundaryErr := s.respondAdapterStreamError(ctx, sse, aerr); boundaryErr != nil {
		s.log.WarnContext(ctx, "adapter.passthrough_override.respond_stream_error_failed", "concern", "adapter.providers.passthrough_override.response", "err", boundaryErr)
		return fmt.Errorf("respond passthrough stream error: %w", boundaryErr)
	}
	return nil
}

// logPassthroughOverrideTerminal emits the success-side terminal log
// event for the passthrough override path. The match between the
// baseline exhaustruct entry and this call site is preserved by
// keeping the field set identical to the previous tail block.
func (s *Server) logPassthroughOverrideTerminal(
	ctx context.Context,
	req *adapterresolver.ResolvedRequest,
	reqID string,
	started time.Time,
	streamRequested bool,
	contentType string,
	usage Usage,
) {
	alias := resolvedRequestAlias(req)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:               adapterruntime.RequestStageCompleted,
		Provider:            providerName(req, ""),
		Backend:             req.Provider.String(),
		RequestID:           reqID,
		Alias:               alias,
		ModelID:             alias,
		Stream:              streamRequested || strings.Contains(contentType, "text/event-stream"),
		TokensIn:            usage.PromptTokens,
		TokensOut:           usage.CompletionTokens,
		CacheReadTokens:     usage.CachedTokens(),
		CacheCreationTokens: 0,
		DurationMs:          clock.Since(started).Milliseconds(),
		Err:                 "",
		FinishReason:        "", DerivedCacheCreationTokens: 0, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false, Correlation: correlation.
					Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	})
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
) ([]byte, int, http.Header, bool) {
	coerced, ok := coercePassthroughOverrideJSON(respBody)
	if ok {
		return coerced, status, hdr, false
	}
	attrs := []slog.Attr{
		slog.String("model", resolvedRequestAlias(req)),
		slog.String("upstream", upstreamLabel),
		slog.Int("first_attempt_bytes", len(respBody)),
	}
	attrs = append(attrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelWarn, structuredOutputPassthroughOverrideParseFailedEvent, append([]slog.Attr{slog.String("concern", slogger.ConcernAdapterProviderPassthroughCoer)}, attrs...)...)
	injectJSONSystemMessage(rawReq, jsonSpec.SystemPrompt(true))
	body2, err := json.Marshal(rawReq)
	if err != nil {
		return respBody, status, hdr, false
	}
	s.recordPassthroughEgress(ctx, firstResponse, firstRequestBody, passthroughCaptureResultFromBody(respBody), started)
	secondStarted := clock.Now()
	secondResponse, err2 := passthroughOverrideDo(ctx, baseURL, apiKey, body2, "/chat/completions")
	if err2 != nil {
		return respBody, status, hdr, true
	}
	defer func() { _ = secondResponse.Body.Close() }()
	rb2, readErr := io.ReadAll(secondResponse.Body)
	if readErr != nil {
		return respBody, status, hdr, true
	}
	s.recordPassthroughEgress(ctx, secondResponse, body2, passthroughCaptureResultFromBody(rb2), secondStarted)
	st2 := secondResponse.StatusCode
	h2 := secondResponse.Header
	if st2 != http.StatusOK {
		return respBody, status, hdr, true
	}
	if c2, ok2 := coercePassthroughOverrideJSON(rb2); ok2 {
		return c2, st2, h2, true
	}
	return rb2, st2, h2, true
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
	aerr := mapUpstreamForFamily(adapterRouteOpenAI, providerName(req, ""), status, codeClass, "", message)
	aerr.Backend = req.Provider.String()
	aerr.ModelAlias = alias
	s.writeShapedError(w, r, aerr)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:        adapterruntime.RequestStageFailed,
		Provider:     providerName(req, ""),
		Backend:      req.Provider.String(),
		RequestID:    reqID,
		Alias:        alias,
		ModelID:      alias,
		Stream:       streamRequested || strings.Contains(contentType, "text/event-stream"),
		DurationMs:   clock.Since(started).Milliseconds(),
		Err:          "upstream returned status " + strconv.Itoa(status),
		FinishReason: "", TokensIn: 0, TokensOut: 0, CacheReadTokens: 0, CacheCreationTokens: 0, DerivedCacheCreationTokens: 0, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false, Correlation: correlation.
				Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	})
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

func injectJSONSystemInstructionIntoFirstMessage(msgs []passthroughOverrideMessage, instruction string) bool {
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

// jsonEncodedString marshals a plain string to its JSON-encoded form
// so it can land in a [json.RawMessage] field on
// passthroughOverrideMessage.
func jsonEncodedString(s string) json.RawMessage {
	encoded, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage("\"\"")
	}
	return encoded
}

// passthroughOverrideDo is the shared transport for Chat Completions and
// Responses passthrough requests. The caller owns and must close resp.Body.
func passthroughOverrideDo(ctx context.Context, baseURL, apiKey string, body []byte, endpointPath string) (*http.Response, error) {
	target := strings.TrimRight(baseURL, "/") + endpointPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		slog.WarnContext(ctx, "adapter.passthrough_override.create_request_failed", "concern", "adapter.providers.passthrough_override.request", "target", target, "err", err)
		return nil, fmt.Errorf("create passthrough override request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post passthrough override request: %w", err)
	}
	return resp, nil
}

func passthroughOverrideUpstreamErrorMessage(status int, body []byte) string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(body), ""))
	if text == "" {
		return "passthrough override upstream returned HTTP " + strconv.Itoa(status) + " with an empty error body"
	}
	const maxLen = 1000
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}
	return "passthrough override upstream returned HTTP " + strconv.Itoa(status) + ": " + text
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
	for i, raw := range choices {
		coerced, ok := coercePassthroughOverrideChoice(raw)
		if !ok {
			allOK = false
		}
		choices[i] = coerced
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

func redactedHeader(name string) bool {
	switch passthroughRedactedHeader(name) {
	case passthroughRedactAuthorization, passthroughRedactProxyAuthorization, passthroughRedactCookie, passthroughRedactSetCookie, passthroughRedactClydeHeader, passthroughRedactAWSSecurityHeader, passthroughRedactOpenAIIdentHeader:
		return true
	case passthroughRedactEmpty:
		return false
	}
	if strings.HasPrefix(name, "x-cursor-") {
		return true
	}
	if strings.HasPrefix(name, "openai-") {
		return true
	}
	return strings.HasSuffix(name, "-api-key")
}

func passthroughOverrideUsageFromBody(body []byte) Usage {
	type wireUsage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		TotalTokens      int `json:"total_tokens"`
		PromptDetails    struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	type responsesEnvelope struct {
		Usage    wireUsage `json:"usage"`
		Response struct {
			Usage wireUsage `json:"usage"`
		} `json:"response"`
	}
	var payload responsesEnvelope
	if err := json.Unmarshal(body, &payload); err != nil {
		for line := range strings.SplitSeq(string(body), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if json.Unmarshal([]byte(candidate), &payload) == nil && (payload.Response.Usage.TotalTokens > 0 || payload.Response.Usage.InputTokens > 0) {
				payload.Usage = payload.Response.Usage
			}
		}
	}
	wire := payload.Usage
	if wire.TotalTokens == 0 && (payload.Response.Usage.TotalTokens > 0 || payload.Response.Usage.InputTokens > 0) {
		wire = payload.Response.Usage
	}
	if wire.InputTokens > 0 {
		wire.PromptTokens = wire.InputTokens
	}
	if wire.OutputTokens > 0 {
		wire.CompletionTokens = wire.OutputTokens
	}
	if wire.PromptTokens == 0 && wire.CompletionTokens == 0 && wire.TotalTokens == 0 {
		return Usage{PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0}
	}
	usage := Usage{
		PromptTokens:     wire.PromptTokens,
		CompletionTokens: wire.CompletionTokens,
		TotalTokens:      wire.TotalTokens, PromptTokensDetails: nil, InputTokens: wire.InputTokens, OutputTokens: wire.OutputTokens, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0,
	}
	cachedTokens := wire.PromptDetails.CachedTokens
	if wire.InputDetails.CachedTokens > 0 {
		cachedTokens = wire.InputDetails.CachedTokens
	}
	if cachedTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: cachedTokens}
	}
	return usage
}
