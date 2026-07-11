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
	rr, parseErr := adapteropenai.UnmarshalResponsesRequest(body)
	if parseErr != nil {
		return adapterErrInvalidJSON("invalid JSON: "+parseErr.Error(), parseErr)
	}
	req, droppedTools, projectionErr := responsesRequestToChatRequest(rr)
	if projectionErr != nil {
		return projectionErr
	}
	forceStreamUsageOptIn(&req)

	resolvedReq, resolverErr := resolveCursorChatRequest(
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
	warnings := adaptercompat.ComputeWarnings(body, resolvedReq.Provider, adaptercompat.EndpointResponses, droppedTools)
	if !warnings.Empty() {
		for _, header := range warnings.Headers() {
			w.Header().Add("X-Clyde-Warning", header)
		}
	}

	s.dispatchResolvedResponses(w, r, req, reqID, resolvedReq, warnings)
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
		Stream:               rr.Stream,
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
		MaxTokens:            nil,
		MaxComplTokens:       nil,
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
		ServiceTier:          rr.ServiceTier,
		Text:                 rr.Text,
		Truncation:           rr.Truncation,
		PromptCacheRetention: rr.PromptCacheRetention,
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

	if strings.TrimSpace(rr.Instructions) != "" {
		req.Messages = append([]ChatMessage{responsesSystemMessage(rr.Instructions)}, req.Messages...)
	}
	if len(req.Messages) == 0 {
		return ChatRequest{}, nil, adapterErrInvalidRequest("input is required", nil)
	}
	return req, droppedTools, nil
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
// output writer differs. Passthrough resolves as an unknown provider
// family here, so it surfaces the unsupported-backend error rather than
// a raw forward.
func (s *Server) dispatchResolvedResponses(
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	reqID string,
	resolvedReq adapterresolver.ResolvedRequest,
	warnings adaptercompat.WarningSet,
) {
	lookupID, known := canonicalProviderID(resolvedReq.Provider)
	if !known {
		s.respondAdapterError(w, r, unsupportedBackendError(&resolvedReq, req.Model))
		return
	}
	provider, lookupErr := s.providerRegistry.Lookup(lookupID)
	if lookupErr != nil || provider == nil {
		s.respondAdapterError(w, r, upstreamUnavailableForProvider(lookupID, &resolvedReq, req.Model))
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
	status := adapteropenai.ResponsesStatusCompleted
	if result.FinishReason == "length" {
		status = adapteropenai.ResponsesStatusIncomplete
	}
	usage := result.Usage
	resp := adapteropenai.BuildResponsesResponse(adapteropenai.ResponsesResponseParams{
		ID:         responseID,
		Model:      alias,
		CreatedAt:  clock.Now().Unix(),
		Status:     status,
		Text:       collected.Text,
		Reasoning:  collected.Reasoning,
		Refusal:    collected.Refusal,
		ToolCalls:  collected.ToolCalls,
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
