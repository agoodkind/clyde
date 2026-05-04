package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"goodkind.io/clyde/internal/adapter/anthropic"
	anthropicbackend "goodkind.io/clyde/internal/adapter/anthropic/backend"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

func (s *Server) dispatchAnthropicProvider(
	w http.ResponseWriter,
	r *http.Request,
	effort string,
	reqID string,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	ctx := anthropic.WithRequestID(r.Context(), reqID)
	var err error
	if resolvedReq.OpenAI.Stream {
		err = s.dispatchAnthropicProviderStream(ctx, w, reqID, resolvedReq)
	} else {
		err = s.dispatchAnthropicProviderCollect(ctx, w, effort, resolvedReq)
	}
	if err != nil {
		s.respondAdapterError(w, r, err)
	}
}

func (s *Server) dispatchAnthropicProviderCollect(
	ctx context.Context,
	w http.ResponseWriter,
	_ string,
	resolvedReq adapterresolver.ResolvedRequest,
) error {
	collector := newProviderCollectorWriter()
	result, runErr := s.anthropicProvider.Execute(ctx, resolvedReq, collector)
	if runErr != nil {
		return anthropicProviderAdapterError(runErr)
	}
	if result.FinalResponse == nil {
		return adapterErrUpstreamFailed("anthropic", "anthropic provider collect path produced no final response", nil)
	}
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(result.UsageNoticeWindows)
	}
	result.UsageNotices = notices
	finalResponse := *result.FinalResponse
	updated, _ := adapterruntime.AppendUsageNoticesToResponse(finalResponse, notices, json.Marshal)
	if len(notices) > 0 {
		finalResponse = updated
	}
	writeJSON(w, http.StatusOK, finalResponse)
	return nil
}

func (s *Server) dispatchAnthropicProviderStream(
	ctx context.Context,
	w http.ResponseWriter,
	reqID string,
	resolvedReq adapterresolver.ResolvedRequest,
) error {
	model := anthropicResolvedModelFromRequest(resolvedReq)
	streamWriter, err := newProviderStreamWriter(ctx, s, w, reqID, model.Alias, "anthropic")
	if err != nil {
		return adapterErrInternal(err.Error(), err)
	}
	s.emitRequestStreamOpened(ctx, model, "oauth", reqID, resolvedReq.Model, true)
	result, runErr := s.anthropicProvider.Execute(ctx, resolvedReq, streamWriter)
	if runErr != nil {
		aerr := anthropicProviderAdapterError(runErr)
		if streamWriter.headersWritten {
			if err := streamWriter.writeStreamErrorBody(aerr.openAIErrorBody()); err != nil {
				s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_error_write_failed",
					slog.String("backend", "anthropic"),
					slog.String("request_id", reqID),
					slog.Any("err", err),
				)
			}
			return nil
		}
		return aerr
	}
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(result.UsageNoticeWindows)
	}
	result.UsageNotices = notices
	if err := streamWriter.finalizeStream(result, resolvedReq.OpenAI.StreamOptions != nil && resolvedReq.OpenAI.StreamOptions.IncludeUsage); err != nil {
		return adapterErrUpstreamFailed("anthropic", err.Error(), err)
	}
	return nil
}

func (s *Server) prepareAnthropicProviderRequest(
	ctx context.Context,
	req adapterresolver.ResolvedRequest,
	reqID string,
) (anthropic.PreparedRequest, error) {
	_ = ctx
	model := anthropicResolvedModelFromRequest(req)
	jsonSpec := ParseResponseFormat(req.OpenAI.ResponseFormat)
	anthReq, err := s.buildAnthropicWire(req.OpenAI, model, req.Effort.String(), jsonSpec, reqID)
	if err != nil {
		return anthropic.PreparedRequest{}, &anthropic.ExecuteError{
			Status:  http.StatusBadRequest,
			Code:    "invalid_request",
			Message: err.Error(),
			Cause:   err,
		}
	}
	jsonCoercion := anthropic.JSONCoercion{}
	if jsonSpec.Mode != "" {
		jsonCoercion = anthropic.JSONCoercion{
			Coerce:   CoerceJSON,
			Validate: LooksLikeJSON,
		}
	}
	return anthropic.PreparedRequest{
		Request:      anthReq,
		Model:        model,
		RequestID:    reqID,
		TrackerKey:   requestContextTrackerKey(req.OpenAI, model.Alias),
		JSONCoercion: jsonCoercion,
		IncludeUsage: req.OpenAI.StreamOptions != nil && req.OpenAI.StreamOptions.IncludeUsage,
		Stream:       req.OpenAI.Stream,
	}, nil
}

func (s *Server) executeAnthropicPreparedRequest(
	ctx context.Context,
	prepared anthropic.PreparedRequest,
	writer adapterprovider.EventWriter,
) (adapterprovider.Result, error) {
	if s.anthr == nil {
		return adapterprovider.Result{}, &anthropic.ExecuteError{
			Status:  http.StatusInternalServerError,
			Code:    "oauth_unconfigured",
			Message: "adapter built without anthropic client; set adapter.direct_oauth=true and restart",
		}
	}
	if err := s.acquire(ctx); err != nil {
		return adapterprovider.Result{}, &anthropic.ExecuteError{
			Status:  http.StatusTooManyRequests,
			Code:    "rate_limited",
			Message: fmt.Sprint(err),
			Cause:   err,
		}
	}
	defer s.release()
	if prepared.Stream || prepared.Request.Stream {
		return s.executeAnthropicPreparedStream(ctx, prepared, writer)
	}
	return s.executeAnthropicPreparedCollect(ctx, prepared, writer)
}

func (s *Server) executeAnthropicPreparedCollect(
	ctx context.Context,
	prepared anthropic.PreparedRequest,
	writer adapterprovider.EventWriter,
) (adapterprovider.Result, error) {
	if prepared.NativeIngress {
		nativeWriter, ok := writer.(*nativeAnthropicJSONWriter)
		if !ok || nativeWriter == nil {
			return adapterprovider.Result{}, &anthropic.ExecuteError{
				Status:  http.StatusInternalServerError,
				Code:    "internal_error",
				Message: "anthropic native collect path requires a native response writer",
			}
		}
		return s.executeAnthropicPreparedCollectNative(ctx, prepared, nativeWriter)
	}
	collector, ok := writer.(*providerCollectorWriter)
	if !ok || collector == nil {
		return adapterprovider.Result{}, &anthropic.ExecuteError{
			Status:  http.StatusInternalServerError,
			Code:    "internal_error",
			Message: "anthropic collect provider requires a collector event writer",
		}
	}
	runtime := &collectResponseDispatcher{server: s, eventWriter: writer}
	reqWithWindows, usageWindows := anthropicRequestWithUsageWindowCapture(prepared.Request)
	started := adapterClock.Now()
	result, err := anthropicbackend.RunCollectExecution(
		runtime,
		ctx,
		reqWithWindows,
		prepared.Model,
		prepared.RequestID,
		started,
		prepared.TrackerKey,
		writer.WriteEvent,
		writer.Flush,
		func() []adapterrender.Event { return collector.events },
	)
	if err != nil {
		return adapterprovider.Result{}, err
	}
	resp := anthropicbackend.MergeCollectedEvents(
		prepared.RequestID,
		prepared.Model.Alias,
		systemFingerprint,
		result.Events,
		result.Usage,
		result.FinishReason,
		anthropicbackend.JSONCoercion{
			Coerce:   prepared.JSONCoercion.Coerce,
			Validate: prepared.JSONCoercion.Validate,
		},
		result.AnthropicStopReason,
	)
	providerResult := anthropicProviderResultFromResponse(&resp)
	providerResult.UsageNoticeWindows = usageWindows()
	return providerResult, nil
}

func (s *Server) executeAnthropicPreparedStream(
	ctx context.Context,
	prepared anthropic.PreparedRequest,
	writer adapterprovider.EventWriter,
) (adapterprovider.Result, error) {
	if prepared.NativeIngress {
		nativeWriter, ok := writer.(*nativeAnthropicStreamWriter)
		if !ok || nativeWriter == nil {
			return adapterprovider.Result{}, &anthropic.ExecuteError{
				Status:  http.StatusInternalServerError,
				Code:    "internal_error",
				Message: "anthropic native stream path requires a native response writer",
			}
		}
		return s.executeAnthropicPreparedStreamNative(ctx, prepared, nativeWriter)
	}
	runtime := &streamResponseDispatcher{server: s, eventWriter: writer}
	reqWithWindows, usageWindows := anthropicRequestWithUsageWindowCapture(prepared.Request)
	started := adapterClock.Now()
	result, err := anthropicbackend.RunStreamExecution(
		runtime,
		ctx,
		reqWithWindows,
		prepared.Model,
		prepared.RequestID,
		started,
		prepared.TrackerKey,
		writer.WriteEvent,
	)
	if err != nil {
		return adapterprovider.Result{}, err
	}
	providerResult := adapterprovider.Result{
		Usage:               result.Usage,
		FinishReason:        result.FinishReason,
		SystemFingerprint:   systemFingerprint,
		ToolCallCount:       result.ToolCallCount,
		ToolCallNames:       result.ToolCallNames,
		HasSubagentToolCall: result.HasSubagentToolCall,
		UsageNoticeWindows:  usageWindows(),
	}
	return providerResult, nil
}

func (s *Server) executeAnthropicPreparedCollectNative(
	ctx context.Context,
	prepared anthropic.PreparedRequest,
	writer *nativeAnthropicJSONWriter,
) (adapterprovider.Result, error) {
	resp, err := s.anthr.Do(ctx, prepared.Request)
	if err != nil {
		return adapterprovider.Result{}, err
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return adapterprovider.Result{}, readErr
	}
	if err := writer.capture(http.StatusOK, resp.Header.Clone(), body); err != nil {
		return adapterprovider.Result{}, err
	}
	return adapterprovider.Result{}, nil
}

func (s *Server) executeAnthropicPreparedStreamNative(
	ctx context.Context,
	prepared anthropic.PreparedRequest,
	writer *nativeAnthropicStreamWriter,
) (adapterprovider.Result, error) {
	resp, err := s.anthr.Do(ctx, prepared.Request)
	if err != nil {
		return adapterprovider.Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := writer.relay(resp); err != nil {
		return adapterprovider.Result{}, err
	}
	return adapterprovider.Result{}, nil
}

func anthropicProviderAdapterError(err error) *adapterError {
	var execErr *anthropic.ExecuteError
	if errors.As(err, &execErr) {
		aerr := adapterErrUpstreamFailed("anthropic", execErr.Message, execErr)
		aerr.HTTPStatus = execErr.Status
		if execErr.Code != "upstream_failed" {
			aerr.OpenAIType = execErr.Code
		}
		aerr.OpenAICode = execErr.Code
		aerr.AnthropicType = anthropicErrorType(execErr.Status, execErr.Code)
		return aerr
	}
	if upstreamErr, ok := anthropic.AsUpstreamError(err); ok {
		status := upstreamErr.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		var aerr *adapterError
		message := upstreamErr.Message
		if message == "" {
			message = upstreamErr.Error()
		}
		switch {
		case upstreamErr.Class() == anthropic.ResponseClassRetryableError && upstreamErr.Status == http.StatusTooManyRequests:
			aerr = newAdapterError(adapterErrorRateLimited, message)
		case upstreamErr.Class() == anthropic.ResponseClassRetryableError:
			aerr = newAdapterError(adapterErrorUpstreamUnavailable, message)
		default:
			aerr = newAdapterError(adapterErrorUpstreamFailed, message)
		}
		aerr.HTTPStatus = status
		aerr.Provider = "anthropic"
		aerr.UpstreamStatus = upstreamErr.Status
		aerr.Cause = err
		return aerr
	}
	return adapterErrUpstreamFailed("anthropic", err.Error(), err)
}

func anthropicResolvedModelFromRequest(req adapterresolver.ResolvedRequest) adaptermodel.ResolvedModel {
	alias := req.Cursor.NormalizedModel
	if alias == "" {
		alias = req.OpenAI.Model
	}
	return adaptermodel.ResolvedModel{
		Alias:           alias,
		Backend:         adaptermodel.BackendAnthropic,
		ClaudeModel:     req.Model,
		Context:         req.ContextBudget.InputTokens,
		Effort:          req.Effort.String(),
		Efforts:         req.Efforts,
		MaxOutputTokens: req.ContextBudget.OutputTokens,
		FamilySlug:      req.Family,
		Thinking:        req.Thinking,
		Instructions:    req.Instructions,
	}
}

func anthropicProviderResultFromResponse(resp *adapteropenai.ChatResponse) adapterprovider.Result {
	if resp == nil {
		return adapterprovider.Result{}
	}
	result := adapterprovider.Result{
		FinalResponse:     resp,
		SystemFingerprint: resp.SystemFingerprint,
	}
	if resp.Usage != nil {
		result.Usage = *resp.Usage
	}
	if len(resp.Choices) > 0 {
		result.FinishReason = resp.Choices[0].FinishReason
		if reasoning := resp.Choices[0].Message.Reasoning; reasoning != "" {
			result.ReasoningSignaled = true
			result.ReasoningVisible = true
			result.ReasoningSummary = reasoning
		}
	}
	return result
}

func anthropicRequestWithUsageWindowCapture(req anthropic.Request) (anthropic.Request, func() []adapterruntime.UsageWindowNoticeInput) {
	var mu sync.Mutex
	var captured []adapterruntime.UsageWindowNoticeInput
	previousOnHeaders := req.OnHeaders
	req.OnHeaders = func(h http.Header) {
		if previousOnHeaders != nil {
			previousOnHeaders(h)
		}
		windows := anthropic.EarlyWarningUsageWindows(h)
		mu.Lock()
		captured = append(captured[:0], windows...)
		mu.Unlock()
	}
	return req, func() []adapterruntime.UsageWindowNoticeInput {
		mu.Lock()
		defer mu.Unlock()
		if len(captured) == 0 {
			return nil
		}
		out := make([]adapterruntime.UsageWindowNoticeInput, len(captured))
		copy(out, captured)
		return out
	}
}

type collectResponseDispatcher struct {
	server      *Server
	eventWriter adapterprovider.EventWriter
}

func (d *collectResponseDispatcher) Log() *slog.Logger {
	return d.server.log
}

func (d *collectResponseDispatcher) EmitRequestStarted(ctx context.Context, model adaptermodel.ResolvedModel, _ string, reqID, modelID string, stream bool) {
	d.server.emitRequestStarted(ctx, model, "oauth", reqID, modelID, stream)
}

func (d *collectResponseDispatcher) EmitRequestStreamOpened(context.Context, adaptermodel.ResolvedModel, string, string, string, bool) {
}

func (d *collectResponseDispatcher) NewAnthropicSSEWriter(http.ResponseWriter) (anthropicbackend.ResponseSSEWriter, error) {
	return nil, fmt.Errorf("anthropic collect dispatcher does not support SSE writers")
}

func (d *collectResponseDispatcher) AnthropicStreamClient() anthropicbackend.StreamClient {
	return d.server.anthr
}

func (d *collectResponseDispatcher) SystemFingerprint() string {
	return systemFingerprint
}

func (d *collectResponseDispatcher) StreamChunkHasVisibleContent(chunk adapteropenai.StreamChunk) bool {
	return streamChunkHasVisibleContent(chunk)
}

func (d *collectResponseDispatcher) WriteEvent(ev adapterrender.Event) error {
	if d == nil || d.eventWriter == nil {
		return nil
	}
	return d.eventWriter.WriteEvent(ev)
}

func (d *collectResponseDispatcher) FlushEventWriter() error {
	if d == nil || d.eventWriter == nil {
		return nil
	}
	return d.eventWriter.Flush()
}

func (d *collectResponseDispatcher) CollectedEvents() []adapterrender.Event {
	collector, ok := d.eventWriter.(*providerCollectorWriter)
	if !ok || collector == nil {
		return nil
	}
	return collector.events
}

func (d *collectResponseDispatcher) TrackAnthropicContextUsage(key string, raw adapteropenai.Usage) anthropicbackend.TrackedUsage {
	tracked := d.server.ctxUsage.Track(key, raw)
	return anthropicbackend.TrackedUsage{
		Usage:      tracked.usage,
		RawPrompt:  tracked.rawPrompt,
		RawTotal:   tracked.rawTotal,
		RolledFrom: tracked.rolledFrom,
	}
}

func (d *collectResponseDispatcher) WriteJSON(_ http.ResponseWriter, _ int, _ adapteropenai.ChatResponse) {
}

func (d *collectResponseDispatcher) WriteErrorJSON(http.ResponseWriter, int, adapteropenai.ErrorResponse) {
}

func (d *collectResponseDispatcher) LogTerminal(ctx context.Context, ev adapterruntime.RequestEvent) {
	adapterruntime.LogTerminal(d.server.log, ctx, d.server.deps.RequestEvents, ev)
}

func (d *collectResponseDispatcher) LogCacheUsageAnthropic(ctx context.Context, backend, reqID, alias string, usage anthropic.Usage) {
	d.server.logCacheUsageAnthropic(ctx, backend, reqID, alias, usage)
}

func (d *collectResponseDispatcher) CacheTTL() string {
	return d.server.cfg.ClientIdentity.PromptCacheTTL
}

type streamResponseDispatcher struct {
	server      *Server
	eventWriter adapterprovider.EventWriter
}

func (d *streamResponseDispatcher) Log() *slog.Logger {
	return d.server.log
}

func (d *streamResponseDispatcher) EmitRequestStarted(ctx context.Context, model adaptermodel.ResolvedModel, _ string, reqID, modelID string, stream bool) {
	d.server.emitRequestStarted(ctx, model, "oauth", reqID, modelID, stream)
}

func (d *streamResponseDispatcher) EmitRequestStreamOpened(ctx context.Context, model adaptermodel.ResolvedModel, _ string, reqID, modelID string, stream bool) {
	d.server.emitRequestStreamOpened(ctx, model, "oauth", reqID, modelID, stream)
}

func (d *streamResponseDispatcher) NewAnthropicSSEWriter(http.ResponseWriter) (anthropicbackend.ResponseSSEWriter, error) {
	return nil, fmt.Errorf("anthropic stream provider dispatch does not expose a backend SSE writer")
}

func (d *streamResponseDispatcher) AnthropicStreamClient() anthropicbackend.StreamClient {
	return d.server.anthr
}

func (d *streamResponseDispatcher) SystemFingerprint() string {
	return systemFingerprint
}

func (d *streamResponseDispatcher) StreamChunkHasVisibleContent(chunk adapteropenai.StreamChunk) bool {
	return streamChunkHasVisibleContent(chunk)
}

func (d *streamResponseDispatcher) WriteEvent(ev adapterrender.Event) error {
	if d == nil || d.eventWriter == nil {
		return nil
	}
	return d.eventWriter.WriteEvent(ev)
}

func (d *streamResponseDispatcher) FlushEventWriter() error {
	if d == nil || d.eventWriter == nil {
		return nil
	}
	return d.eventWriter.Flush()
}

func (d *streamResponseDispatcher) CollectedEvents() []adapterrender.Event {
	return nil
}

func (d *streamResponseDispatcher) TrackAnthropicContextUsage(key string, raw adapteropenai.Usage) anthropicbackend.TrackedUsage {
	tracked := d.server.ctxUsage.Track(key, raw)
	return anthropicbackend.TrackedUsage{
		Usage:      tracked.usage,
		RawPrompt:  tracked.rawPrompt,
		RawTotal:   tracked.rawTotal,
		RolledFrom: tracked.rolledFrom,
	}
}

func (d *streamResponseDispatcher) WriteJSON(_ http.ResponseWriter, _ int, _ adapteropenai.ChatResponse) {
}

func (d *streamResponseDispatcher) WriteErrorJSON(_ http.ResponseWriter, _ int, _ adapteropenai.ErrorResponse) {
}

func (d *streamResponseDispatcher) LogTerminal(ctx context.Context, ev adapterruntime.RequestEvent) {
	adapterruntime.LogTerminal(d.server.log, ctx, d.server.deps.RequestEvents, ev)
}

func (d *streamResponseDispatcher) LogCacheUsageAnthropic(ctx context.Context, backend, reqID, alias string, usage anthropic.Usage) {
	d.server.logCacheUsageAnthropic(ctx, backend, reqID, alias, usage)
}

func (d *streamResponseDispatcher) CacheTTL() string {
	return d.server.cfg.ClientIdentity.PromptCacheTTL
}
