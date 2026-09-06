package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
	"goodkind.io/gklog/trace"
)

// handleResponses serves POST /v1/responses. It parses the typed
// Responses request, projects it into the shared ChatRequest the resolve
// pipeline consumes, runs the same resolve + preflight the chat path
// runs, then dispatches to the resolved provider with a Responses writer
// so the provider's normalized event stream renders as the Responses
// response object (non-streaming) or the Responses SSE sequence
// (streaming). It reuses the OpenAI route family, so pre-headers errors
// flow through the existing OpenAI error boundary.
func (s *Server) handleResponses(ctx context.Context, hctx *handlerCtx) (err error) {
	defer trace.Op(ctx, "adapter.openai.responses")(&err)
	started := clock.Now()
	w := hctx.Writer
	r := hctx.Request
	if r.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}
	corr := hctx.Correlation
	reqID := corr.RequestID
	responseID := responsesResponseID(reqID)
	clydeingress.SetHTTPHeaders(corr, w.Header())

	wireBody, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxResponsesRequestBodyBytes))
	if readErr != nil {
		return adapterErrInvalidRequest("failed to read body", readErr)
	}
	if capW := s.beginIngressCapture(hctx); capW != nil {
		w = hctx.Writer
		defer func() {
			s.finishIngressCapture(capW, hctx.Correlation, r, wireBody, started, err)
		}()
	}
	body, decodeErr := readResponsesRequestBody(wireBody, r.Header.Get("Content-Encoding"))
	if decodeErr != nil {
		return adapterErrInvalidRequest("invalid zstd request body", decodeErr)
	}
	handledNative, nativeErr := s.tryDispatchNativeCodexResponses(ctx, w, r, reqID, wireBody, body, corr)
	if nativeErr != nil {
		return nativeErr
	}
	if handledNative {
		return nil
	}
	rr, parseErr := adapteropenai.UnmarshalResponsesRequest(body)
	if parseErr != nil {
		return adapterErrInvalidJSON("invalid JSON: "+parseErr.Error(), parseErr)
	}
	if rejected := adaptercompat.RejectedParam(func(param string) int { return int(rr.Fields.Presence(param)) }); rejected != "" {
		return adapterErrInvalidRequest(rejected+" is not supported by Clyde", nil)
	}
	req, droppedTools, projectionErr := responsesRequestToChatRequest(rr)
	if projectionErr != nil {
		return projectionErr
	}
	forceStreamUsageOptIn(&req)

	resolvedReq, resolverErr := resolveResponsesRequest(
		openAIIngressSurface(ctx),
		req,
		adapterresolver.NewModelRegistryAdapter(s.modelRegistry()),
	)
	if resolverErr != nil {
		var invalidRequestErr *adapterresolver.InvalidRequestError
		if errors.As(resolverErr, &invalidRequestErr) {
			return adapterErrInvalidRequest(invalidRequestErr.Error(), resolverErr)
		}
		return adapterErrModelNotFound(resolverErr.Error())
	}
	resolvedReq.RequestID = reqID
	resolvedReq.Correlation = corr
	resolvedReq.Responses = &rr
	resolvedReq.ResponsesFields = rr.Fields

	if overrideErr := s.applyBackendOverride(r, req, &resolvedReq, reqID); overrideErr != nil {
		return overrideErr
	}
	if preErr := s.preflightChat(ctx, &req, &resolvedReq, reqID); preErr != nil {
		return preErr
	}
	if resolvedReq.Provider == adapterresolver.ProviderPassthrough {
		s.forwardPassthroughResponsesWire(w, r, reqID, &resolvedReq, wireBody, body)
		return nil
	}

	// The compatibility boundary describes which request fields the resolved
	// provider omits or overrides, plus the built-in / custom tool types the
	// projection dropped. It reads the raw body for top-level field presence
	// and never performs the omission itself.
	warningValues := adaptercompat.ResponsesWarningValues{N: rr.N, ToolChoice: rr.ToolChoice}
	warnings := adaptercompat.ComputeWarningsFromResponsesPresence(func(param string) int { return int(rr.Fields.Presence(param)) }, warningValues, resolvedReq.Provider, adaptercompat.EndpointResponses, droppedTools)

	s.dispatchResolvedResponsesWithID(w, r, req, reqID, responseID, body, resolvedReq, warnings)
	return nil
}

func (s *Server) tryDispatchNativeCodexResponses(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	requestID string,
	wireBody []byte,
	decodedBody []byte,
	corr correlation.Context,
) (bool, error) {
	raw, model, native := nativeCodexResponsesRequest(wireBody, decodedBody, r.Header, corr)
	if !native {
		return false, nil
	}
	rr, parseErr := adapteropenai.UnmarshalResponsesRequest(decodedBody)
	if parseErr != nil {
		return false, adapterErrInvalidJSON("invalid JSON: "+parseErr.Error(), parseErr)
	}
	if rejected := adaptercompat.RejectedParam(func(param string) int { return int(rr.Fields.Presence(param)) }); rejected != "" {
		return false, adapterErrInvalidRequest(rejected+" is not supported by Clyde", nil)
	}
	req, _, projectionErr := responsesRequestToChatRequest(rr)
	if projectionErr != nil {
		decodedRaw := raw
		decodedRaw.Body = decodedBody
		if !adaptercodex.HasRawResponsesCompactionItem(decodedRaw) {
			return false, projectionErr
		}
		var rawCompactionRequest ChatRequest
		rawCompactionRequest.Model = model
		req = rawCompactionRequest
	}
	forceStreamUsageOptIn(&req)
	resolvedReq, resolverErr := resolveResponsesRequest(
		openAIIngressSurface(ctx), req, adapterresolver.NewModelRegistryAdapter(s.modelRegistry()),
	)
	if resolverErr != nil {
		return false, nativeResponsesResolverError(resolverErr)
	}
	resolvedReq.RequestID = requestID
	resolvedReq.Correlation = corr
	resolvedReq.Responses = &rr
	resolvedReq.ResponsesFields = rr.Fields
	if overrideErr := s.applyBackendOverride(r, req, &resolvedReq, requestID); overrideErr != nil {
		return false, overrideErr
	}
	if resolvedReq.Provider != adapterresolver.ProviderCodex {
		return false, nil
	}
	if preErr := s.preflightChat(ctx, &req, &resolvedReq, requestID); preErr != nil {
		return false, preErr
	}
	resolvedRaw := raw
	resolvedBody := decodedBody
	if model != resolvedReq.Model {
		decodedRaw := raw
		decodedRaw.Body = decodedBody
		rewrittenRaw, rawErr := decodedRaw.MarshalWithModel(resolvedReq.Model)
		if rawErr != nil {
			return false, adapterErrInvalidRequest(rawErr.Error(), rawErr)
		}
		resolvedBody = rewrittenRaw.Body
		forwardBody, encoded := encodeNativeResponsesBody(resolvedBody, raw.Header.Get("Content-Encoding"))
		if !encoded {
			return false, adapterErrInvalidRequest("failed to encode resolved Responses request", nil)
		}
		resolvedRaw = rewrittenRaw
		resolvedRaw.Body = forwardBody
	}
	compactionSettings := s.deps.RawResponsesCompaction
	compactionSettings.ContextWindowTokens = resolvedReq.ContextBudget.InputTokens
	transformedRaw, compactionTransformer, v2Plan, v2Recovery := prepareNativeCodexResponsesCompaction(resolvedRaw, resolvedBody, compactionSettings, s.compactionV2)
	s.dispatchNativeCodexResponses(w, r, requestID, transformedRaw, resolvedReq, compactionTransformer, v2Plan, v2Recovery)
	return true, nil
}

func nativeResponsesResolverError(resolverErr error) *adapterError {
	var invalidRequestErr *adapterresolver.InvalidRequestError
	if errors.As(resolverErr, &invalidRequestErr) {
		return adapterErrInvalidRequest(invalidRequestErr.Error(), resolverErr)
	}
	return adapterErrModelNotFound(resolverErr.Error())
}

// forwardPassthroughResponsesWire retains native request compression when no
// rewrite is required, and re-encodes a rewritten model under that encoding.
func (s *Server) forwardPassthroughResponsesWire(w http.ResponseWriter, r *http.Request, reqID string, req *adapterresolver.ResolvedRequest, wireBody, decodedBody []byte) {
	baseURL, apiKey, modelOverride, upstreamLabel, targetErr := passthroughUpstreamTarget(req)
	if targetErr != nil {
		s.respondAdapterError(w, r, targetErr)
		return
	}
	contentEncoding := r.Header.Get("Content-Encoding")
	body := wireBody
	if !nativeResponsesZstdEncoded(contentEncoding) {
		body = decodedBody
		contentEncoding = ""
	}
	if modelOverride != "" {
		rewrittenBody := passthroughResponsesBodyWithModel(decodedBody, modelOverride)
		encodedBody, encoded := encodeNativeResponsesBody(rewrittenBody, contentEncoding)
		if !encoded {
			s.respondAdapterError(w, r, adapterErrInvalidRequest("failed to encode passthrough Responses request", nil))
			return
		}
		body = encodedBody
	}
	streamRequested := passthroughBodyStreamRequested(decodedBody)
	s.forwardPassthroughHTTP(w, r, req, passthroughForwardOptions{
		requestID: reqID, endpointPath: "/responses", baseURL: baseURL, apiKey: apiKey,
		upstreamLabel: upstreamLabel, body: body, contentEncoding: contentEncoding,
		streamRequested: streamRequested, streamIncrementally: true, preserveCorrelation: true,
		rawChatRequest: nil, jsonSpec: JSONResponseSpec{Mode: "", SchemaName: "", Schema: nil},
	})
}

func nativeCodexResponsesRequest(wireBody, decodedBody []byte, header http.Header, corr correlation.Context) (adaptercodex.RawResponsesRequest, string, bool) {
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(decodedBody, &request); err != nil || strings.TrimSpace(request.Model) == "" {
		var zero adaptercodex.RawResponsesRequest
		return zero, "", false
	}
	raw := adaptercodex.RawResponsesRequest{
		Body: wireBody, Header: header.Clone(), RequestID: corr.RequestID, Correlation: corr, Stream: request.Stream,
	}
	if !raw.HasValidTurnMetadata() {
		var zero adaptercodex.RawResponsesRequest
		return zero, "", false
	}
	return raw, request.Model, true
}

func (s *Server) dispatchNativeCodexResponses(
	w http.ResponseWriter,
	r *http.Request,
	requestID string,
	raw adaptercodex.RawResponsesRequest,
	resolved adapterresolver.ResolvedRequest,
	compactionTransformer *adaptercodex.RawResponsesCompactionTransformer,
	v2Plan *adaptercodex.RawResponsesCompactionV2Plan,
	v2Recovery *adaptercodex.RawResponsesCompactionV2Recovery,
) {
	if s.codexProvider == nil {
		if v2Recovery != nil {
			v2Recovery.ReleaseRecovery()
		}
		s.respondAdapterError(w, r, codexProviderAdapterError(adaptercodex.ErrCodexProviderNotConfigured))
		return
	}
	ctx, lifecycle := s.beginProviderRequestLifecycle(r.Context(), &resolved, "direct", requestID, resolved.Model, raw.Stream)
	egressCtx, releaseEgress := s.codexRawResponsesEgressContext(ctx, requestID)
	defer releaseEgress("codex.raw_responses.done")
	response, err := s.codexProvider.OpenRawResponses(egressCtx, raw)
	if err != nil {
		if v2Recovery != nil {
			v2Recovery.ReleaseRecovery()
		}
		var result adapterprovider.Result
		lifecycle.terminal(ctx, result, err)
		s.respondAdapterError(w, r, codexProviderAdapterError(err))
		return
	}
	streamingResponse := raw.Stream || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	if compactionTransformer != nil {
		response = transformNativeCodexCompactionResponse(response, compactionTransformer, streamingResponse)
	}
	if v2Plan != nil {
		response = adaptercodex.ObserveRawResponsesCompactionV2Response(response, *v2Plan, s.compactionV2)
	}
	defer func() { _ = response.Body.Close() }()
	if streamingResponse {
		lifecycle.streamOpened(ctx)
	}
	_, copyErr := s.copyPassthroughResponse(ctx, w, response, streamingResponse)
	if copyErr == nil && v2Plan != nil {
		adaptercodex.ArmRawResponsesCompactionV2Response(response)
	}
	var result adapterprovider.Result
	lifecycle.terminal(ctx, result, copyErr)
	if copyErr != nil {
		s.log.WarnContext(ctx, "adapter.codex.raw_responses.copy_failed", "concern", "adapter.providers.codex.request", "err", copyErr)
	}
}

// responsesRequestToChatRequest projects a typed Responses request into
// the ChatRequest the shared resolve pipeline consumes. It carries the
// model, streaming flag, reasoning, sampling, and Responses input fields
// through unchanged, folds `input` into messages, and prepends
// `instructions` as a system message. It splits the raw Responses tools
// array leniently, keeping client function tools and returning the type
// labels of any built-in or custom tools it dropped so the caller can
// warn about them.
func responsesRequestToChatRequest(rr adapteropenai.ResponsesRequest) (ChatRequest, []string, *adapterError) {
	if strings.TrimSpace(rr.Model) == "" {
		return ChatRequest{}, nil, adapterErrInvalidRequest("model is required", nil)
	}
	functionTools, droppedTools := adapteropenai.SplitResponsesTools(rr.Tools)
	req := ChatRequest{
		Model:                rr.Model,
		Messages:             nil,
		Input:                rr.Input,
		Stream:               rr.Stream != nil && *rr.Stream,
		StreamOptions:        nil,
		ReasoningEffort:      "",
		Reasoning:            rr.Reasoning,
		Tools:                functionTools,
		ToolChoice:           rr.ToolChoice,
		Functions:            nil,
		FunctionCall:         nil,
		N:                    0,
		User:                 "",
		Temperature:          rr.Temperature,
		TopP:                 rr.TopP,
		MaxTokens:            rr.MaxTokens,
		MaxComplTokens:       rr.MaxCompletionTokens,
		MaxOutputTokens:      rr.MaxOutputTokens,
		PresencePenalty:      nil,
		FrequencyPenalty:     nil,
		LogitBias:            nil,
		Logprobs:             nil,
		TopLogprobs:          nil,
		Stop:                 rr.Stop,
		Seed:                 nil,
		ResponseFormat:       nil,
		Audio:                nil,
		Modalities:           nil,
		ParallelTools:        rr.ParallelTools,
		Store:                rr.Store,
		Metadata:             rr.Metadata,
		Include:              rr.Include,
		ServiceTier:          responsesString(rr.ServiceTier),
		Text:                 rr.Text,
		Truncation:           responsesString(rr.Truncation),
		PromptCacheRetention: responsesString(rr.PromptCacheRetention),
	}

	// The Responses input may be a bare JSON string (a single user turn)
	// or the array-of-items shape normalizeRequestMessages folds. Handle
	// the string form here; leave the array form for the shared folder.
	trimmed := strings.TrimSpace(string(rr.Input))
	if len(trimmed) > 0 && trimmed[0] == '"' {
		req.Input = nil
		req.Messages = []ChatMessage{responsesUserMessage(json.RawMessage(trimmed))}
	}

	if normErr := normalizeRequestMessages(&req); normErr != nil {
		var aerr *adapterError
		if errors.As(normErr, &aerr) {
			return ChatRequest{}, nil, aerr
		}
		return ChatRequest{}, nil, adapterErrInvalidRequest(normErr.Error(), normErr)
	}

	if instructions := responsesString(rr.Instructions); strings.TrimSpace(instructions) != "" {
		req.Messages = append([]ChatMessage{responsesSystemMessage(instructions)}, req.Messages...)
	}
	if len(req.Messages) == 0 {
		return ChatRequest{}, nil, adapterErrInvalidRequest("input is required", nil)
	}
	return req, droppedTools, nil
}

func responsesString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// responsesSystemMessage builds the system ChatMessage the projection
// prepends from the Responses `instructions` field.
func responsesSystemMessage(instructions string) ChatMessage {
	return ChatMessage{
		Role:    "system",
		Content: json.RawMessage(strconv.Quote(instructions)),
		Name:    "", ToolCalls: nil, ToolCallID: "", Reasoning: "", ReasoningContent: "", Refusal: "", Annotations: nil,
	}
}

// responsesUserMessage builds the user ChatMessage the projection uses
// when Responses `input` arrives as a bare JSON string.
func responsesUserMessage(content json.RawMessage) ChatMessage {
	return ChatMessage{
		Role:    "user",
		Content: content,
		Name:    "", ToolCalls: nil, ToolCallID: "", Reasoning: "", ReasoningContent: "", Refusal: "", Annotations: nil,
	}
}

// responsesResponseID derives the stable resp_ id from the correlation
// request id so the streamed events and the terminal object share one
// id without calling rand directly.
func responsesResponseID(reqID string) string {
	core := strings.TrimPrefix(reqID, "chatcmpl-")
	if core == "" {
		core = reqID
	}
	return "resp_" + core
}

func (s *Server) dispatchResolvedResponsesWithID(
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	reqID string,
	responseID string,
	body []byte,
	resolvedReq adapterresolver.ResolvedRequest,
	warnings adaptercompat.WarningSet,
) {
	if resolvedReq.Provider == adapterresolver.ProviderPassthrough {
		s.forwardPassthroughResponses(w, r, reqID, &resolvedReq, body)
		return
	}
	alias := resolvedRequestAlias(&resolvedReq)
	if !warnings.Empty() {
		for _, header := range warnings.Headers() {
			w.Header().Add("X-Clyde-Warning", header)
		}
	}
	providerContext := responsesProviderContext(r.Context(), resolvedReq)
	prepared, prepareErr := s.prepareResponsesProvider(providerContext, resolvedReq)
	if prepareErr != nil {
		mappedErr := responsesPreparedProviderError(resolvedReq.Provider, alias, resolvedReq, prepareErr)
		mappedErr.Warnings = warnings.Slice()
		s.respondAdapterError(w, r, mappedErr)
		return
	}
	warningSlice := warnings.Slice()
	if req.Stream {
		s.dispatchResponsesStream(w, r, responseID, alias, resolvedReq, prepared, warningSlice)
		return
	}
	s.dispatchResponsesCollect(w, r, responseID, alias, resolvedReq, prepared, warningSlice)
}

// dispatchResponsesStream runs the provider with the streaming Responses
// writer. response.created and response.in_progress are emitted before
// Execute; a mid-stream provider error surfaces as response.failed
// because the headers are already committed.
func (s *Server) dispatchResponsesStream(
	w http.ResponseWriter,
	r *http.Request,
	responseID string,
	alias string,
	resolvedReq adapterresolver.ResolvedRequest,
	prepared preparedResponsesProvider,
	warnings []adaptercompat.CompatibilityWarning,
) {
	ctx := responsesProviderContext(r.Context(), resolvedReq)
	writer, err := newResponsesStreamWriter(w, responseID, alias, warnings, s.log)
	if err != nil {
		s.respondAdapterError(w, r, adapterErrInternal(err.Error(), err))
		return
	}
	if beginErr := writer.begin(); beginErr != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.responses.begin_failed", slog.String("concern", "adapter.chat.render"), slog.String("request_id", resolvedReq.RequestID),
			slog.String("model", alias),
			slog.Any("err", beginErr),
		)
		return
	}
	ctx, lifecycle := s.beginProviderRequestLifecycle(
		ctx,
		&resolvedReq,
		responsesProviderPath(prepared.provider),
		resolvedReq.RequestID,
		resolvedReq.Model,
		true,
	)
	lifecycle.streamOpened(ctx)
	result, runErr := prepared.Execute(ctx, writer, s)
	lifecycle.terminal(ctx, result, runErr)
	if runErr != nil {
		mappedErr := responsesPreparedProviderError(prepared.provider, alias, resolvedReq, runErr)
		if failErr := writer.fail(mappedErr); failErr != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.responses.fail_write_failed", slog.String("concern", "adapter.chat.render"), slog.String("request_id", resolvedReq.RequestID),
				slog.String("model", alias),
				slog.Any("err", failErr),
			)
		}
		return
	}
	if finishErr := writer.finish(result); finishErr != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.responses.finish_failed", slog.String("concern", "adapter.chat.render"), slog.String("request_id", resolvedReq.RequestID),
			slog.String("model", alias),
			slog.Any("err", finishErr),
		)
	}
}

// dispatchResponsesCollect runs the provider with the buffering collector
// writer, then assembles the non-streaming Responses response object from
// the collected events and provider result.
func (s *Server) dispatchResponsesCollect(
	w http.ResponseWriter,
	r *http.Request,
	responseID string,
	alias string,
	resolvedReq adapterresolver.ResolvedRequest,
	prepared preparedResponsesProvider,
	warnings []adaptercompat.CompatibilityWarning,
) {
	ctx := responsesProviderContext(r.Context(), resolvedReq)
	collector := newProviderCollectorWriter()
	ctx, lifecycle := s.beginProviderRequestLifecycle(
		ctx,
		&resolvedReq,
		responsesProviderPath(prepared.provider),
		resolvedReq.RequestID,
		resolvedReq.Model,
		false,
	)
	result, runErr := prepared.Execute(ctx, collector, s)
	lifecycle.terminal(ctx, result, runErr)
	if runErr != nil {
		mappedErr := responsesPreparedProviderError(prepared.provider, alias, resolvedReq, runErr)
		mappedErr.Warnings = warnings
		s.respondAdapterError(w, r, mappedErr)
		return
	}
	collected := adapterrender.CollectMessageWithNativePatchRepresentation(
		collector.events,
		nativePatchRepresentationForResolvedCursorRoute(resolvedReq),
	)
	text, reasoning, refusal, toolCalls := collected.Text, collected.Reasoning, collected.Refusal, collected.ToolCalls
	usage := result.Usage
	// A provider that assembles the completion itself (the Anthropic
	// non-streaming path) returns the ChatResponse in result.FinalResponse and
	// writes no render events, so the collector is empty. Build the Responses
	// object from the final response in that case.
	if result.FinalResponse != nil {
		text, reasoning, refusal, toolCalls = responsesFieldsFromChatResponse(result.FinalResponse)
		if result.FinalResponse.Usage != nil {
			usage = *result.FinalResponse.Usage
		}
	}
	status, incompleteDetails := adapteropenai.ResponsesTerminalForFinishReason(result.FinishReason)
	output := responsesOutputFromEvents(responseID, collector.events, status)
	if result.FinalResponse != nil {
		output = nil
	}
	resp := adapteropenai.BuildResponsesResponse(adapteropenai.ResponsesResponseParams{
		ID:         responseID,
		Model:      alias,
		CreatedAt:  clock.Now().Unix(),
		Status:     status,
		Text:       text,
		Reasoning:  reasoning,
		Refusal:    refusal,
		ToolCalls:  toolCalls,
		Output:     output,
		Usage:      &usage,
		ItemIDBase: responsesItemBase(responseID),
		Warnings:   warnings,
	})
	resp.IncompleteDetails = incompleteDetails
	body, marshalErr := json.Marshal(resp)
	if marshalErr != nil {
		s.respondAdapterError(w, r, adapterErrInternal("marshal responses object", marshalErr))
		return
	}
	writeJSON(w, body)
}

// responsesFieldsFromChatResponse extracts the assistant text, reasoning,
// refusal, and tool calls from a provider-assembled ChatResponse. Providers
// that build the non-streaming completion themselves (the Anthropic path)
// return it in Result.FinalResponse and write no render events, so the
// Responses object is assembled from the final response instead of the
// collected event stream.
func responsesFieldsFromChatResponse(resp *adapteropenai.ChatResponse) (text, reasoning, refusal string, toolCalls []adapteropenai.ToolCall) {
	if resp == nil || len(resp.Choices) == 0 {
		return "", "", "", nil
	}
	message := resp.Choices[0].Message
	reasoning = message.Reasoning
	if reasoning == "" {
		reasoning = message.ReasoningContent
	}
	return responsesChatMessageText(message.Content), reasoning, message.Refusal, message.ToolCalls
}

// responsesChatMessageText extracts only literal text from a string or typed
// content-part array. Non-text parts never become synthetic output markers.
func responsesChatMessageText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return ""
	}
	var parts []adapteropenai.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == "text" {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}
