package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

var errPassthroughRequestFailed = errors.New("passthrough request failed")

const passthroughSystemRole = "system"

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

// passthroughBodyStreamRequested reports whether the body requests streaming.
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

// respondPassthroughOverrideTransportError handles the transport-level
// failure case where the upstream call never produced a status. The
// envelope is shaped through the boundary so Cursor BYOK still
// renders the upstream connection error rather than a fallback
// message.
func (s *Server) respondPassthroughOverrideTransportError(
	writer http.ResponseWriter,
	request *http.Request,
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
	s.respondAdapterError(writer, request, aerr)
}

// writePassthroughOverrideResponse forwards a 2xx upstream body to the
// caller verbatim. The caller flow has already early-returned on
// non-2xx so this writer only ever sees a status that is already in
// the OpenAI compat success window.
func writePassthroughOverrideResponse(
	corr correlation.Context,
	writer http.ResponseWriter,
	status int,
	respBody []byte,
	hdr http.Header,
) {
	copyPassthroughHeaders(writer.Header(), hdr)
	clydeingress.SetHTTPHeaders(corr, writer.Header())
	writer.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	writer.WriteHeader(status)
	_, _ = writer.Write(respBody)
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

func (s *Server) respondPassthroughStreamCopyError(
	ctx context.Context,
	writer http.ResponseWriter,
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
	sse, sseErr := adapteropenai.NewSSEWriter(writer)
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
		Role:    passthroughSystemRole,
		Content: jsonEncodedString(instruction),
	}
	msgs = append([]passthroughOverrideMessage{sys}, msgs...)
	encoded, err := json.Marshal(msgs)
	if err == nil {
		req["messages"] = encoded
	}
}

func injectJSONSystemInstructionIntoFirstMessage(
	msgs []passthroughOverrideMessage,
	instruction string,
) bool {
	if len(msgs) == 0 {
		return false
	}

	first := msgs[0]
	if first.Role != passthroughSystemRole && first.Role != "developer" {
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
		sanitizedErr := fmt.Errorf(
			"create passthrough override request for %s failed: %w",
			safeTarget,
			errPassthroughRequestFailed,
		)
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
		sanitizedErr := fmt.Errorf(
			"post passthrough override request to %s failed: %w",
			safeTarget,
			errPassthroughRequestFailed,
		)
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
		sanitizedErr := fmt.Errorf(
			"parse passthrough target %s failed: %w",
			safeTarget,
			errPassthroughRequestFailed,
		)
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

// coercePassthroughOverrideJSON applies CoerceJSON to the OpenAI response
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

	for index, raw := range choices {
		coerced, ok := coercePassthroughOverrideChoice(raw)
		if !ok {
			allOK = false
		}

		choices[index] = coerced
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

	rawMessage, exists := fields["message"]
	if !exists {
		return raw, true
	}

	var msgFields map[string]json.RawMessage
	if err := json.Unmarshal(rawMessage, &msgFields); err != nil {
		return raw, true
	}

	rawContent, exists := msgFields["content"]
	if !exists {
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
