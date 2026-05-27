package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
)

const structuredOutputPassthroughOverrideParseFailedEvent = "passthrough_override structured-output parse failed; retrying"

func (s *Server) forwardPassthroughOverride(w http.ResponseWriter, r *http.Request, model ResolvedModel, body []byte) {
	started := adapterClock.Now()
	reqID := newRequestID()
	corr := correlation.FromContext(r.Context()).Child().WithRequestID(reqID)
	if corr.TraceID == "" {
		corr = clydeingress.FromHTTPHeader(r.Header, reqID)
	}
	clydeingress.SetHTTPHeaders(corr, w.Header())
	ctx := correlation.WithContext(r.Context(), corr)
	r = r.WithContext(ctx)
	streamRequested := false
	baseURL := strings.TrimSpace(model.OpenAICompatPassthrough.BaseURL)
	apiKey := model.OpenAICompatPassthrough.APIKey
	apiKeyEnv := model.OpenAICompatPassthrough.APIKeyEnv
	modelOverride := model.OpenAICompatPassthrough.Model
	upstreamLabel := "openai_compat_passthrough"
	if baseURL == "" {
		override, ok := s.registry.PassthroughOverride(model.PassthroughOverride)
		if !ok || override.BaseURL == "" {
			err := newAdapterError(adapterErrorUpstreamUnavailable,
				"alias routes to passthrough override "+model.PassthroughOverride+" but no base URL is configured")
			err.Provider = providerName(model, "")
			err.Backend = model.Backend
			err.ModelAlias = model.Alias
			s.respondAdapterError(w, r, err)
			return
		}
		baseURL = override.BaseURL
		apiKey = override.APIKey
		apiKeyEnv = override.APIKeyEnv
		modelOverride = override.Model
		upstreamLabel = "passthrough_override:" + model.PassthroughOverride
	}
	if apiKey == "" && apiKeyEnv != "" {
		apiKey = os.Getenv(apiKeyEnv)
	}

	rawReq, jsonSpec, body, streamRequested := mutatePassthroughOverrideRequestBody(body, modelOverride, streamRequested)
	s.emitRequestStarted(ctx, model, "", reqID, model.Alias, streamRequested)

	respBody, status, hdr, err := passthroughOverrideCall(ctx, baseURL, apiKey, body)
	if err != nil {
		s.respondPassthroughOverrideTransportError(w, r, ctx, model, reqID, started, streamRequested, err)
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(hdr.Get("Content-Type")))
	if streamRequested || strings.Contains(contentType, "text/event-stream") {
		s.emitRequestStreamOpened(ctx, model, "", reqID, model.Alias, true)
	}

	if jsonSpec.Mode != "" && status == http.StatusOK {
		respBody, status, hdr = s.coerceOrRetryPassthroughOverrideJSON(
			ctx, corr, model, upstreamLabel, baseURL, apiKey, rawReq, jsonSpec, respBody, status, hdr,
		)
	}
	if status >= http.StatusBadRequest {
		s.respondPassthroughOverrideError(w, r, ctx, model, reqID, status, respBody, streamRequested, contentType, started)
		return
	}

	writePassthroughOverrideResponse(w, status, respBody, hdr)

	usage := passthroughOverrideUsageFromBody(respBody)
	s.logPassthroughOverrideTerminal(ctx, model, reqID, started, streamRequested, contentType, usage)
}

// mutatePassthroughOverrideRequestBody applies the alias rewrite and
// json-spec extraction in one place so forwardPassthroughOverride
// stays under the funlen budget. Returns the parsed map (or nil),
// the parsed json spec, the (possibly rewritten) request bytes, and
// whether the request asked for a streaming response.
func mutatePassthroughOverrideRequestBody(body []byte, modelOverride string, streamRequested bool) (map[string]any, JSONResponseSpec, []byte, bool) {
	rawReq := map[string]any{}
	jsonSpec := JSONResponseSpec{}
	if err := json.Unmarshal(body, &rawReq); err != nil {
		return nil, jsonSpec, body, streamRequested
	}
	if v, ok := rawReq["stream"].(bool); ok {
		streamRequested = v
	}
	if modelOverride != "" {
		rawReq["model"] = modelOverride
	}
	if rf, ok := rawReq["response_format"]; ok {
		rfBytes, _ := json.Marshal(rf)
		jsonSpec = ParseResponseFormat(rfBytes)
	}
	if jsonSpec.Mode != "" {
		injectJSONSystemMessage(rawReq, jsonSpec.SystemPrompt(false))
		delete(rawReq, "response_format")
	}
	rewritten, _ := json.Marshal(rawReq)
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
	model ResolvedModel,
	reqID string,
	started time.Time,
	streamRequested bool,
	err error,
) {
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:      adapterruntime.RequestStageFailed,
		Provider:   providerName(model, ""),
		Backend:    model.Backend,
		RequestID:  reqID,
		Alias:      model.Alias,
		ModelID:    model.Alias,
		Stream:     streamRequested,
		DurationMs: time.Since(started).Milliseconds(),
		Err:        err.Error(),
	})
	aerr := newAdapterError(adapterErrorUpstreamUnavailable, err.Error())
	aerr.Provider = providerName(model, "")
	aerr.Backend = model.Backend
	aerr.ModelAlias = model.Alias
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
	model ResolvedModel,
	reqID string,
	started time.Time,
	streamRequested bool,
	contentType string,
	usage Usage,
) {
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:               adapterruntime.RequestStageCompleted,
		Provider:            providerName(model, ""),
		Backend:             model.Backend,
		RequestID:           reqID,
		Alias:               model.Alias,
		ModelID:             model.Alias,
		Stream:              streamRequested || strings.Contains(contentType, "text/event-stream"),
		TokensIn:            usage.PromptTokens,
		TokensOut:           usage.CompletionTokens,
		CacheReadTokens:     usage.CachedTokens(),
		CacheCreationTokens: 0,
		DurationMs:          time.Since(started).Milliseconds(),
		Err:                 "",
	})
}

// coerceOrRetryPassthroughOverrideJSON folds the JSON-spec coercion
// and one-shot retry path into a helper so forwardPassthroughOverride
// stays under the funlen budget. Returns the (possibly rewritten)
// body, status, and headers.
func (s *Server) coerceOrRetryPassthroughOverrideJSON(
	ctx context.Context,
	corr correlation.Context,
	model ResolvedModel,
	upstreamLabel string,
	baseURL string,
	apiKey string,
	rawReq map[string]any,
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
		slog.String("model", model.Alias),
		slog.String("upstream", upstreamLabel),
		slog.Int("first_attempt_bytes", len(respBody)),
	}
	attrs = append(attrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelWarn, structuredOutputPassthroughOverrideParseFailedEvent, attrs...)
	injectJSONSystemMessage(rawReq, jsonSpec.SystemPrompt(true))
	body2, _ := json.Marshal(rawReq)
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
	model ResolvedModel,
	reqID string,
	status int,
	respBody []byte,
	streamRequested bool,
	contentType string,
	started time.Time,
) {
	message := passthroughOverrideUpstreamErrorMessage(status, respBody)
	codeClass := anthropicCodeClassForStatus(status)
	aerr := mapUpstreamForFamily(adapterRouteOpenAI, providerName(model, ""), status, codeClass, "", message)
	aerr.Backend = model.Backend
	aerr.ModelAlias = model.Alias
	s.writeShapedError(w, r, aerr)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:      adapterruntime.RequestStageFailed,
		Provider:   providerName(model, ""),
		Backend:    model.Backend,
		RequestID:  reqID,
		Alias:      model.Alias,
		ModelID:    model.Alias,
		Stream:     streamRequested || strings.Contains(contentType, "text/event-stream"),
		DurationMs: time.Since(started).Milliseconds(),
		Err:        "upstream returned status " + strconv.Itoa(status),
	})
}

// injectJSONSystemMessage prepends (or appends to existing system
// content) an instruction telling the model to emit raw JSON only.
func injectJSONSystemMessage(req map[string]any, instruction string) {
	if instruction == "" {
		return
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) > 0 {
		first, _ := msgs[0].(map[string]any)
		if first != nil {
			role, _ := first["role"].(string)
			if role == "system" || role == "developer" {
				if existing, ok := first["content"].(string); ok {
					first["content"] = instruction + "\n\n" + existing
				} else {
					first["content"] = instruction
				}
				msgs[0] = first
				req["messages"] = msgs
				return
			}
		}
	}
	sys := map[string]any{"role": "system", "content": instruction}
	req["messages"] = append([]any{sys}, msgs...)
}

// passthroughOverrideCall posts body to the upstream chat/completions endpoint and
// returns body+status+headers.
func passthroughOverrideCall(ctx context.Context, baseURL, apiKey string, body []byte) ([]byte, int, http.Header, error) {
	target := strings.TrimRight(baseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header, err
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
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return body, false
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return body, false
	}
	allOK := true
	for i, c := range choices {
		choice, _ := c.(map[string]any)
		if choice == nil {
			allOK = false
			continue
		}
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			continue
		}
		content, _ := msg["content"].(string)
		if content == "" {
			continue
		}
		coerced := CoerceJSON(content)
		if !LooksLikeJSON(coerced) {
			allOK = false
			continue
		}
		msg["content"] = coerced
		choice["message"] = msg
		choices[i] = choice
	}
	resp["choices"] = choices
	out, _ := json.Marshal(resp)
	return out, allOK
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

func redactedHeader(name string) bool {
	switch name {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-clyde-token", "x-amz-security-token", "openai-api-key":
		return true
	case "":
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
		return Usage{}
	}
	usage := Usage{
		PromptTokens:     payload.Usage.PromptTokens,
		CompletionTokens: payload.Usage.CompletionTokens,
		TotalTokens:      payload.Usage.TotalTokens,
	}
	if payload.Usage.PromptDetails.CachedTokens > 0 {
		usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: payload.Usage.PromptDetails.CachedTokens}
	}
	return usage
}
