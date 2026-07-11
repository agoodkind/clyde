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

	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
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
	clydeingress.SetHTTPHeaders(corr, w.Header())

	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if readErr != nil {
		return adapterErrInvalidRequest("failed to read body", readErr)
	}
	if capW := s.beginIngressCapture(hctx); capW != nil {
		w = hctx.Writer
		defer func() {
			s.finishIngressCapture(capW, hctx.Correlation, r, body, started, err)
		}()
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

	// The compatibility boundary describes which request fields the resolved
	// provider omits or overrides, plus the built-in / custom tool types the
	// projection dropped. It reads the raw body for top-level field presence
	// and never performs the omission itself.
	warnings := adaptercompat.ComputeWarningsFromResponsesPresence(func(param string) int { return int(rr.Fields.Presence(param)) }, rr.N, resolvedReq.Provider, adaptercompat.EndpointResponses, droppedTools)
	if !warnings.Empty() {
		for _, header := range warnings.Headers() {
			w.Header().Add("X-Clyde-Warning", header)
		}
	}

	s.dispatchResolvedResponses(w, r, req, reqID, body, resolvedReq, warnings)
	return nil
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

// dispatchResolvedResponses looks up the resolved provider and runs it
// with a Responses writer. It mirrors dispatchResolvedChat's provider
// lookup so responses and chat share the same backend contract, but the
// output writer differs. Passthrough is a raw HTTP forward, handled
// before the registry lookup, and targets the upstream's /responses
// endpoint with the raw body.
func (s *Server) dispatchResolvedResponses(
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	reqID string,
	body []byte,
	resolvedReq adapterresolver.ResolvedRequest,
	warnings adaptercompat.WarningSet,
) {
	if resolvedReq.Provider == adapterresolver.ProviderPassthrough {
		s.forwardPassthroughResponses(w, r, reqID, &resolvedReq, body)
		return
	}
	provider, lookupAdapterErr := s.lookupResolvedProvider(&resolvedReq, req.Model)
	if lookupAdapterErr != nil {
		s.respondAdapterError(w, r, lookupAdapterErr)
		return
	}
	responseID := responsesResponseID(reqID)
	alias := resolvedRequestAlias(&resolvedReq)
	warningSlice := warnings.Slice()
	if req.Stream {
		s.dispatchResponsesStream(w, r, responseID, alias, resolvedReq, provider, warningSlice)
		return
	}
	s.dispatchResponsesCollect(w, r, responseID, alias, resolvedReq, provider, warningSlice)
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
	provider adapterprovider.Provider,
	warnings []adaptercompat.CompatibilityWarning,
) {
	ctx := r.Context()
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
	result, runErr := provider.Execute(ctx, resolvedReq, writer)
	if runErr != nil {
		if failErr := writer.fail(runErr); failErr != nil {
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
	provider adapterprovider.Provider,
	warnings []adaptercompat.CompatibilityWarning,
) {
	ctx := r.Context()
	collector := newProviderCollectorWriter()
	result, runErr := provider.Execute(ctx, resolvedReq, collector)
	if runErr != nil {
		aerr := mapUpstreamForFamily(adapterRouteOpenAI, provider.ID().String(), 0, upstreamClassServerError, "", runErr.Error())
		aerr.Backend = resolvedReq.Provider.String()
		aerr.ModelAlias = alias
		aerr.ResolvedModelName = resolvedReq.Model
		aerr.Cause = runErr
		s.respondAdapterError(w, r, aerr)
		return
	}
	collected := adapterrender.CollectMessage(collector.events)
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
	status := adapteropenai.ResponsesStatusCompleted
	if result.FinishReason == "length" {
		status = adapteropenai.ResponsesStatusIncomplete
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
		Usage:      &usage,
		ItemIDBase: responsesItemBase(responseID),
		Warnings:   warnings,
	})
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
