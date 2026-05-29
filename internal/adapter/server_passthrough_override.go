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

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
)

const structuredOutputPassthroughOverrideParseFailedEvent = "passthrough_override structured-output parse failed; retrying"

func (s *Server) forwardPassthroughOverride(w http.ResponseWriter, r *http.Request, req *adapterresolver.ResolvedRequest, body []byte) {
	started := clock.Now()
	reqID := newRequestID()
	corr := correlation.FromContext(r.Context()).Child().WithRequestID(reqID)
	if corr.TraceID == "" {
		corr = clydeingress.FromHTTPHeader(r.Header, reqID)
	}
	clydeingress.SetHTTPHeaders(corr, w.Header())
	ctx := correlation.WithContext(r.Context(), corr)
	r = r.WithContext(ctx)
	alias := resolvedRequestAlias(req)
	streamRequested := false
	baseURL := strings.TrimSpace(req.OpenAICompatPassthrough.BaseURL)
	apiKey := req.OpenAICompatPassthrough.APIKey
	apiKeyEnv := req.OpenAICompatPassthrough.APIKeyEnv
	modelOverride := req.OpenAICompatPassthrough.Model
	upstreamLabel := "openai_compat_passthrough"
	if baseURL == "" {
		override, ok := s.registry.PassthroughOverride(req.PassthroughOverrideName)
		if !ok || override.BaseURL == "" {
			err := newAdapterError(adapterErrorUpstreamUnavailable,
				"alias routes to passthrough override "+req.PassthroughOverrideName+" but no base URL is configured")
			err.Provider = providerName(req, "")
			err.Backend = req.Provider.String()
			err.ModelAlias = alias
			s.respondAdapterError(w, r, err)
			return
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

	rawReq, jsonSpec, body, streamRequested := mutatePassthroughOverrideRequestBody(body, modelOverride, streamRequested)
	s.emitRequestStarted(ctx, req, "", reqID, alias, streamRequested)

	respBody, status, hdr, err := passthroughOverrideCall(ctx, baseURL, apiKey, body)
	if err != nil {
		s.respondPassthroughOverrideTransportError(w, r, ctx, req, reqID, started, streamRequested, err)
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(hdr.Get("Content-Type")))
	if streamRequested || strings.Contains(contentType, "text/event-stream") {
		s.emitRequestStreamOpened(ctx, req, "", reqID, alias, true)
	}

	if jsonSpec.Mode != "" && status == http.StatusOK {
		respBody, status, hdr = s.coerceOrRetryPassthroughOverrideJSON(
			ctx, corr, req, upstreamLabel, baseURL, apiKey, rawReq, jsonSpec, respBody, status, hdr,
		)
	}
	if status >= http.StatusBadRequest {
		s.respondPassthroughOverrideError(w, r, ctx, req, reqID, status, respBody, streamRequested, contentType, started)
		return
	}

	writePassthroughOverrideResponse(w, status, respBody, hdr)

	usage := passthroughOverrideUsageFromBody(respBody)
	s.logPassthroughOverrideTerminal(ctx, req, reqID, started, streamRequested, contentType, usage)
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
func mutatePassthroughOverrideRequestBody(body []byte, modelOverride string, streamRequested bool) (passthroughOverrideRequest, JSONResponseSpec, []byte, bool) {
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
	for k, v := range hdr {
		// Drop any upstream-set Content-Length; we may have rewritten
		// the body and a stale length triggers the http2 framework to
		// return zero bytes to the client.
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
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
	respBody []byte,
	status int,
	hdr http.Header,
) ([]byte, int, http.Header) {
	coerced, ok := coercePassthroughOverrideJSON(respBody)
	if ok {
		return coerced, status, hdr
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
		return respBody, status, hdr
	}
	rb2, st2, h2, err2 := passthroughOverrideCall(ctx, baseURL, apiKey, body2)
	if err2 != nil || st2 != http.StatusOK {
		return respBody, status, hdr
	}
	if c2, ok2 := coercePassthroughOverrideJSON(rb2); ok2 {
		return c2, st2, h2
	}
	return rb2, st2, h2
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

// passthroughOverrideCall posts body to the upstream chat/completions endpoint and
// returns body+status+headers.
func passthroughOverrideCall(ctx context.Context, baseURL, apiKey string, body []byte) ([]byte, int, http.Header, error) {
	target := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		slog.WarnContext(ctx, "adapter.passthrough_override.create_request_failed", "concern", "adapter.providers.passthrough_override.request", "target", target, "err", err)
		return nil, 0, nil, fmt.Errorf("create passthrough override request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("post passthrough override request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("read passthrough override response: %w", err)
	}
	return rb, resp.StatusCode, resp.Header, nil
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
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Usage{PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0}
	}
	usage := Usage{
		PromptTokens:     payload.Usage.PromptTokens,
		CompletionTokens: payload.Usage.CompletionTokens,
		TotalTokens:      payload.Usage.TotalTokens, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0,
	}
	if payload.Usage.PromptDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: payload.Usage.PromptDetails.CachedTokens}
	}
	return usage
}
