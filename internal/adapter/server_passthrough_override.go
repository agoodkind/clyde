package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
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
	apiKeyFile := req.OpenAICompatPassthrough.APIKeyFile
	setting := "adapter.openai_compat_passthrough"
	modelOverride = req.OpenAICompatPassthrough.Model
	upstreamLabel = "openai_compat_passthrough"
	if req.PassthroughOverrideName != "" {
		override := req.PassthroughOverride
		if override.BaseURL == "" {
			e := newAdapterError(
				adapterErrorUpstreamUnavailable,
				"alias routes to passthrough override "+req.PassthroughOverrideName+" but no base URL is configured",
			)
			e.Provider = providerName(req, "")
			e.Backend = req.Provider.String()
			e.ModelAlias = resolvedRequestAlias(req)

			return "", "", "", "", e
		}

		baseURL = override.BaseURL
		apiKey = override.APIKey
		apiKeyEnv = override.APIKeyEnv
		apiKeyFile = override.APIKeyFile
		setting = "adapter.passthrough_overrides." + req.PassthroughOverrideName
		modelOverride = override.Model
		upstreamLabel = "passthrough_override:" + req.PassthroughOverrideName
	}

	resolvedKey, err := config.ResolvePassthroughAPIKey(setting, apiKey, apiKeyEnv, apiKeyFile)
	if err != nil {
		return "", "", "", "", newAdapterError(adapterErrorUpstreamUnavailable, err.Error())
	}

	return baseURL, resolvedKey, modelOverride, upstreamLabel, nil
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
	ctx := correlation.WithContext(r.Context(), corr)
	if adapterruntime.ExecutionIDFromContext(ctx) == "" {
		ctx = adapterruntime.WithExecutionID(ctx, newExecutionID())
	}

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
		respBody := []byte(nil)
		s.recordPassthroughEgress(
			ctx,
			resp,
			options.body,
			passthroughCaptureResultFromBody(respBody),
			started,
		)
		s.respondPassthroughOverrideError(
			w,
			r,
			ctx,
			req,
			options.requestID,
			resp.StatusCode,
			respBody,
			options.streamRequested,
			contentType,
			started,
		)

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
		copyResult, copyErr := s.copyPassthroughResponse(ctx, w, resp, true)
		s.recordPassthroughEgress(ctx, resp, options.body, copyResult, started)
		if copyErr != nil {
			s.log.WarnContext(
				ctx,
				"adapter.passthrough_override.copy_response_failed",
				"concern",
				"adapter.providers.passthrough_override.response",
				"err",
				copyErr,
			)
			if boundaryErr := s.respondPassthroughStreamCopyError(
				ctx,
				w,
				req,
				options.requestID,
				started,
				copyErr,
			); boundaryErr != nil {
				s.log.WarnContext(
					ctx,
					"adapter.passthrough_override.stream_error_boundary_failed",
					"concern",
					"adapter.providers.passthrough_override.response",
					"err",
					boundaryErr,
				)
			}

			return
		}

		s.logPassthroughOverrideTerminal(
			ctx,
			req,
			options.requestID,
			started,
			options.streamRequested,
			contentType,
			copyResult.usage,
		)

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

	status := resp.StatusCode
	header := resp.Header
	if options.jsonSpec.Mode != "" && status == http.StatusOK {
		var captureRecorded bool
		var retryFailure *passthroughRetryFailure
		respBody, status, header, captureRecorded, retryFailure = s.coerceOrRetryPassthroughOverrideJSON(
			ctx,
			corr,
			req,
			options.upstreamLabel,
			options.baseURL,
			options.apiKey,
			options.rawChatRequest,
			options.jsonSpec,
			resp,
			options.body,
			respBody,
			status,
			header,
			started,
		)
		if retryFailure != nil {
			s.respondPassthroughRetryFailure(
				w,
				r,
				ctx,
				req,
				options.requestID,
				started,
				options.streamRequested,
				retryFailure,
			)

			return
		}

		if !captureRecorded {
			s.recordPassthroughEgress(
				ctx,
				resp,
				options.body,
				passthroughCaptureResultFromBody(respBody),
				started,
			)
		}
	} else {
		s.recordPassthroughEgress(
			ctx,
			resp,
			options.body,
			passthroughCaptureResultFromBody(respBody),
			started,
		)
	}

	writePassthroughOverrideResponse(corr, w, status, respBody, header)
	s.logPassthroughOverrideTerminal(
		ctx,
		req,
		options.requestID,
		started,
		options.streamRequested,
		contentType,
		passthroughOverrideUsageFromBody(respBody),
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

func passthroughCaptureResultFromBody(body []byte) passthroughCaptureResult {
	return passthroughCaptureResult{
		body:       body,
		totalBytes: len(body),
		truncated:  false,
		usage:      passthroughOverrideUsageFromBody(body),
	}
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

func passthroughStreamCaptureResult(
	captured *capture.CappedBuffer,
	usageParser *passthroughSSEUsageParser,
	interrupted bool,
) passthroughCaptureResult {
	return passthroughCaptureResult{
		body:       captured.Bytes(),
		totalBytes: captured.TotalRead(),
		truncated:  captured.Truncated() || interrupted,
		usage:      usageParser.Usage(),
	}
}

func copyPassthroughHeaders(dst, src http.Header) {
	connectionHeaders := passthroughConnectionHeaders(src)
	for key, values := range src {
		if connectionHeaders[strings.ToLower(key)] {
			continue
		}

		switch passthroughFramingHeader(http.CanonicalHeaderKey(key)) {
		case passthroughHeaderConnection,
			passthroughHeaderKeepAlive,
			passthroughHeaderProxyAuthenticate,
			passthroughHeaderProxyAuthorization,
			passthroughHeaderTE,
			passthroughHeaderTrailer,
			passthroughHeaderTransferEncoding,
			passthroughHeaderUpgrade,
			passthroughHeaderContentLength,
			passthroughHeaderProxyConnection:
			continue
		default:
		}

		if passthroughClydeCorrelationHeader(key) {
			continue
		}

		dst[key] = values
	}
}

func passthroughConnectionHeaders(headers http.Header) map[string]bool {
	nominated := make(map[string]bool)

	for key, values := range headers {
		if !strings.EqualFold(key, string(passthroughHeaderConnection)) {
			continue
		}

		for _, value := range values {
			for name := range strings.SplitSeq(value, ",") {
				trimmed := strings.TrimSpace(name)
				if trimmed != "" {
					nominated[strings.ToLower(trimmed)] = true
				}
			}
		}
	}

	return nominated
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
	adapterruntime.LogTerminal(
		s.log,
		ctx,
		s.deps.RequestEvents,
		adapterruntime.RequestEvent{
			Stage:       adapterruntime.RequestStageCompleted,
			Provider:    providerName(req, ""),
			Backend:     req.Provider.String(),
			RequestID:   reqID,
			ExecutionID: adapterruntime.ExecutionIDFromContext(ctx),
			Alias:       alias,
			ModelID:     alias,
			Stream: streamRequested ||
				strings.Contains(contentType, "text/event-stream"),
			TokensIn:                   usage.PromptTokens,
			TokensOut:                  usage.CompletionTokens,
			CacheReadTokens:            usage.CachedTokens(),
			CacheCreationTokens:        0,
			DurationMs:                 clock.Since(started).Milliseconds(),
			Err:                        "",
			FinishReason:               "",
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
	s.log.LogAttrs(
		ctx,
		slog.LevelWarn,
		structuredOutputPassthroughOverrideParseFailedEvent,
		append(
			[]slog.Attr{slog.String("concern", slogger.ConcernAdapterProviderPassthroughCoer)},
			attrs...,
		)...,
	)
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
