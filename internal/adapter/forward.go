package adapter

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
)

func (s *Server) forwardPassthroughStream(
	ctx context.Context,
	writer http.ResponseWriter,
	req *adapterresolver.ResolvedRequest,
	options *passthroughForwardOptions,
	resp *http.Response,
	started time.Time,
	contentType string,
) {
	copyResult, copyErr := s.copyPassthroughResponse(ctx, writer, resp, true)
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
			writer,
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
}

func (s *Server) finishPassthroughResponse(
	ctx context.Context,
	corr correlation.Context,
	writer http.ResponseWriter,
	request *http.Request,
	req *adapterresolver.ResolvedRequest,
	options *passthroughForwardOptions,
	resp *http.Response,
	respBody *[]byte,
	started time.Time,
	contentType string,
) {
	status := resp.StatusCode
	header := resp.Header
	if options.jsonSpec.Mode != "" && status == http.StatusOK {
		var captureRecorded bool
		var retryFailure *passthroughRetryFailure
		*respBody, status, header, captureRecorded, retryFailure = s.coerceOrRetryPassthroughOverrideJSON(
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
			*respBody,
			status,
			header,
			started,
		)
		if retryFailure != nil {
			s.respondPassthroughRetryFailure(
				writer,
				request,
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
				passthroughCaptureResultFromBody(*respBody),
				started,
			)
		}
	} else {
		s.recordPassthroughEgress(
			ctx,
			resp,
			options.body,
			passthroughCaptureResultFromBody(*respBody),
			started,
		)
	}

	writePassthroughOverrideResponse(corr, writer, status, *respBody, header)
	s.logPassthroughOverrideTerminal(
		ctx,
		req,
		options.requestID,
		started,
		options.streamRequested,
		contentType,
		passthroughOverrideUsageFromBody(*respBody),
	)
}

func (s *Server) logPassthroughJSONRetry(ctx context.Context, attrs []slog.Attr) {
	s.log.LogAttrs(
		ctx,
		slog.LevelWarn,
		structuredOutputPassthroughOverrideParseFailedEvent,
		append(
			[]slog.Attr{slog.String("concern", slogger.ConcernAdapterProviderPassthroughCoer)},
			attrs...,
		)...,
	)
}

func (s *Server) logPassthroughOverrideFailure(
	ctx context.Context,
	req *adapterresolver.ResolvedRequest,
	reqID string,
	started time.Time,
	stream bool,
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
			Stream:                     stream,
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
			Correlation:                correlation.Context{},
		},
	)
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

func passthroughCaptureResultFromBody(body []byte) passthroughCaptureResult {
	return passthroughCaptureResult{
		body:       body,
		totalBytes: len(body),
		truncated:  false,
		usage:      passthroughOverrideUsageFromBody(body),
	}
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
		if encodedReasoning, ok := passthroughReasoningWithWireEffort(
			rawReasoning,
			encodedEffort,
		); ok {
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

func sanitizedPassthroughTarget(rawTarget string) string {
	target, err := url.Parse(rawTarget)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return "configured passthrough target"
	}

	target.User = nil
	target.RawQuery = ""
	target.ForceQuery = false
	target.Fragment = ""

	return target.Scheme + "://" + target.Host + target.EscapedPath()
}

func passthroughOverrideUpstreamErrorMessage(status int, body []byte) string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(body), ""))
	if text == "" {
		return "passthrough override upstream returned HTTP " + strconv.Itoa(
			status,
		) + " with an empty error body"
	}

	const maxLen = 1000
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}

	return "passthrough override upstream returned HTTP " + strconv.Itoa(status) + ": " + text
}

func redactedHeader(name string) bool {
	switch passthroughRedactedHeader(name) {
	case passthroughRedactAuthorization,
		passthroughRedactProxyAuthorization,
		passthroughRedactCookie,
		passthroughRedactSetCookie,
		passthroughRedactClydeHeader,
		passthroughRedactAWSSecurityHeader,
		passthroughRedactOpenAIIdentHeader:
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

	return strings.HasSuffix(name, "-api-key") || name == "chatgpt-"+"account-id"
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
			if json.Unmarshal([]byte(candidate), &payload) == nil &&
				(payload.Response.Usage.TotalTokens > 0 || payload.Response.Usage.InputTokens > 0) {
				payload.Usage = payload.Response.Usage
			}
		}
	}

	wire := payload.Usage
	if wire.TotalTokens == 0 &&
		(payload.Response.Usage.TotalTokens > 0 || payload.Response.Usage.InputTokens > 0) {
		wire = payload.Response.Usage
	}

	if wire.InputTokens > 0 {
		wire.PromptTokens = wire.InputTokens
	}

	if wire.OutputTokens > 0 {
		wire.CompletionTokens = wire.OutputTokens
	}

	if wire.PromptTokens == 0 && wire.CompletionTokens == 0 && wire.TotalTokens == 0 {
		return Usage{
			PromptTokens:            0,
			CompletionTokens:        0,
			TotalTokens:             0,
			PromptTokensDetails:     nil,
			CompletionTokensDetails: nil,
			InputTokens:             0,
			OutputTokens:            0,
			CacheReadTokens:         0,
			CacheWriteTokens:        0,
			MaxTokens:               0,
		}
	}

	usage := Usage{
		PromptTokens:            wire.PromptTokens,
		CompletionTokens:        wire.CompletionTokens,
		TotalTokens:             wire.TotalTokens,
		PromptTokensDetails:     nil,
		CompletionTokensDetails: nil,
		InputTokens:             wire.InputTokens,
		OutputTokens:            wire.OutputTokens,
		CacheReadTokens:         0,
		CacheWriteTokens:        0,
		MaxTokens:               0,
	}
	cachedTokens := wire.PromptDetails.CachedTokens
	if wire.InputDetails.CachedTokens > 0 {
		cachedTokens = wire.InputDetails.CachedTokens
	}

	if cachedTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens:               cachedTokens,
			CacheWriteTokens:           nil,
			CompatibilityOmitsUncached: false,
		}
	}

	return usage
}

func (s *Server) respondPassthroughInformational(
	ctx context.Context,
	writer http.ResponseWriter,
	request *http.Request,
	req *adapterresolver.ResolvedRequest,
	options *passthroughForwardOptions,
	resp *http.Response,
	started time.Time,
	contentType string,
) {
	respBody := []byte(nil)
	s.recordPassthroughEgress(
		ctx,
		resp,
		options.body,
		passthroughCaptureResultFromBody(respBody),
		started,
	)
	s.respondPassthroughOverrideError(
		writer,
		request,
		ctx,
		req,
		options.requestID,
		resp.StatusCode,
		respBody,
		options.streamRequested,
		contentType,
		started,
	)
}

func newPassthroughContext(parent context.Context, corr correlation.Context) context.Context {
	ctx := correlation.WithContext(parent, corr)
	if adapterruntime.ExecutionIDFromContext(ctx) == "" {
		ctx = adapterruntime.WithExecutionID(ctx, newExecutionID())
	}

	return ctx
}
