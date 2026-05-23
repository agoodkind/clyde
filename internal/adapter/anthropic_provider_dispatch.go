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
	updated, _ := adapterruntime.AppendUsageNoticesToResponse(ctx, finalResponse, notices, json.Marshal)
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
	includeUsage := resolvedReq.OpenAI.StreamOptions != nil && resolvedReq.OpenAI.StreamOptions.IncludeUsage
	// Anthropic streams sometimes end with a non-nil runErr after the
	// answer text has fully streamed (a late SSE error frame, a
	// scanner error, or a non-clean upstream close). When that
	// happens the OpenAI finalize sequence is still required so
	// Cursor and other OpenAI-SDK clients receive a finish chunk, a
	// usage chunk, and the [DONE] terminator. Codex's dispatcher
	// always finalizes when content was written; the Anthropic path
	// now mirrors that. The Claude Code reference SSE consumer
	// applies the same fallback by deriving stop_reason and using
	// the cumulative usage when message_stop never arrives.
	if runErr != nil {
		return s.handleAnthropicStreamRunErr(ctx, streamWriter, model, reqID, runErr, result, includeUsage)
	}
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(result.UsageNoticeWindows)
	}
	result.UsageNotices = notices
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.anthropic_stream_finalized",
		slog.String("backend", "anthropic"),
		slog.String("request_id", reqID),
		slog.String("model", model.Alias),
		slog.Bool("include_usage", includeUsage),
		slog.String("finish_reason", finishReason),
		slog.Int("usage_total_tokens", result.Usage.TotalTokens),
	)
	if err := streamWriter.finalizeStream(ctx, result, includeUsage); err != nil {
		return adapterErrUpstreamFailed("anthropic", err.Error(), err)
	}
	return nil
}

// handleAnthropicStreamRunErr decides between the pre-content error
// path and the post-content finalize-with-partial-result path when the
// Anthropic provider's Execute returns a non-nil error from a streaming
// turn. The streamWriter.headersWritten flag is the boundary: when no
// content has reached the wire we surface a Cursor-safe error envelope,
// and when content has streamed we still emit the OpenAI finalize
// sequence with the partial usage and recovered finish_reason.
func (s *Server) handleAnthropicStreamRunErr(
	ctx context.Context,
	streamWriter *providerStreamWriter,
	model adaptermodel.ResolvedModel,
	reqID string,
	runErr error,
	result adapterprovider.Result,
	includeUsage bool,
) error {
	aerr := anthropicProviderAdapterError(runErr)
	if !streamWriter.headersWritten {
		s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.anthropic_stream_pre_content_error",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("model", model.Alias),
			slog.String("run_err", sanitizeAnthropicRunErr(runErr)),
			slog.Bool("include_usage", includeUsage),
		)
		return aerr
	}
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(result.UsageNoticeWindows)
	}
	result.UsageNotices = notices
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.anthropic_stream_finalized_after_runerr",
		slog.String("backend", "anthropic"),
		slog.String("request_id", reqID),
		slog.String("model", model.Alias),
		slog.String("run_err", sanitizeAnthropicRunErr(runErr)),
		slog.Bool("include_usage", includeUsage),
		slog.Bool("headers_written", streamWriter.headersWritten),
		slog.String("finish_reason", finishReason),
		slog.Int("usage_total_tokens", result.Usage.TotalTokens),
	)
	if err := streamWriter.finalizeStream(ctx, result, includeUsage); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_finalize_after_runerr_failed",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.Any("err", err),
		)
	}
	return nil
}

// sanitizeAnthropicRunErr renders a provider error to a short string
// without leaking response bodies, prompts, tokens, or other sensitive
// data. Only the error type and a fixed-width prefix of the message are
// retained so the log line is queryable but bounded.
func sanitizeAnthropicRunErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	const limit = 200
	if len(msg) > limit {
		msg = msg[:limit] + "..."
	}
	return fmt.Sprintf("%T: %s", err, msg)
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
		return adapterprovider.Result{}, s.executeAnthropicPreparedCollectNative(ctx, prepared, nativeWriter)
	}
	collector, ok := writer.(*providerCollectorWriter)
	if !ok || collector == nil {
		return adapterprovider.Result{}, &anthropic.ExecuteError{
			Status:  http.StatusInternalServerError,
			Code:    "internal_error",
			Message: "anthropic collect provider requires a collector event writer",
		}
	}
	// Register this outbound Anthropic call in the egress registry so
	// the daemon reload deadline can force-close a wedged HTTP
	// connection. The context cancel from registerEgress propagates
	// through http.Do so the body read unblocks when force-close fires.
	egressCtx, _, releaseEgress := registerEgress(ctx, s.egressRegistry, egressSessionKindHTTP, EgressMeta{
		Provider:          "anthropic",
		UpstreamURL:       s.anthr.MessagesURL(),
		UpstreamRequestID: "",
		AttemptNo:         0,
		ParentRequestID:   prepared.RequestID,
	})
	defer releaseEgress("anthropic.collect.done")

	runtime := &collectResponseDispatcher{server: s}
	reqWithWindows, usageWindows := anthropicRequestWithUsageWindowCapture(prepared.Request)
	started := adapterClock.Now()
	result, err := anthropicbackend.RunCollectExecution(
		runtime,
		egressCtx,
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
	// Register this outbound Anthropic SSE call in the egress registry.
	// The context cancel propagates through the SSE scan goroutine so
	// force-close under the reload deadline unblocks a wedged reader.
	egressCtx, _, releaseEgress := registerEgress(ctx, s.egressRegistry, egressSessionKindHTTP, EgressMeta{
		Provider:          "anthropic",
		UpstreamURL:       s.anthr.MessagesURL(),
		UpstreamRequestID: "",
		AttemptNo:         0,
		ParentRequestID:   prepared.RequestID,
	})
	defer releaseEgress("anthropic.stream.done")

	runtime := &streamResponseDispatcher{server: s}
	reqWithWindows, usageWindows := anthropicRequestWithUsageWindowCapture(prepared.Request)
	started := adapterClock.Now()
	result, err := anthropicbackend.RunStreamExecution(
		runtime,
		egressCtx,
		reqWithWindows,
		prepared.Model,
		prepared.RequestID,
		started,
		prepared.TrackerKey,
		writer.WriteEvent,
	)
	providerResult := adapterprovider.Result{
		Usage:               result.Usage,
		FinishReason:        result.FinishReason,
		SystemFingerprint:   systemFingerprint,
		ToolCallCount:       result.ToolCallCount,
		ToolCallNames:       result.ToolCallNames,
		HasSubagentToolCall: result.HasSubagentToolCall,
		UsageNoticeWindows:  usageWindows(),
	}
	if err != nil {
		return providerResult, err
	}
	return providerResult, nil
}

func (s *Server) executeAnthropicPreparedCollectNative(
	ctx context.Context,
	prepared anthropic.PreparedRequest,
	writer *nativeAnthropicJSONWriter,
) error {
	resp, err := s.anthr.Do(ctx, prepared.Request)
	if err != nil {
		return err
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	return writer.capture(http.StatusOK, resp.Header.Clone(), body)
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
		message := upstreamErr.Message
		if message == "" {
			message = upstreamErr.Error()
		}
		// Generalize the previous inline 429 workaround through the
		// shared mapping helper. Every non-2xx upstream from the
		// Anthropic provider flows through mapUpstreamForFamily so
		// Cursor BYOK gets a parseable invalid_request envelope with
		// the upstream message preserved verbatim.
		//
		// ErrorType takes precedence over Status because an Anthropic
		// 200 stream can carry a typed rejection in its first SSE
		// `event: error` frame (CLYDE-439); the wire HTTP status is
		// 200 in that case, so a status-only switch would misroute
		// the failure as upstream_failed. When ErrorType is empty
		// the call failed at the HTTP layer and Status carries the
		// signal.
		codeClass := upstreamClassUnknown
		switch {
		case upstreamErr.ErrorType == anthropic.ErrorKindRateLimit:
			codeClass = upstreamClassRateLimit
		case upstreamErr.ErrorType == anthropic.ErrorKindOverloaded:
			codeClass = upstreamClassServerError
		case upstreamErr.ErrorType == anthropic.ErrorKindAuth:
			codeClass = upstreamClassAuth
		case upstreamErr.ErrorType == anthropic.ErrorKindInvalidRequest:
			codeClass = upstreamClassInvalidRequest
		case upstreamErr.ErrorType == anthropic.ErrorKindAPI:
			codeClass = upstreamClassServerError
		case upstreamErr.Status == http.StatusTooManyRequests:
			codeClass = upstreamClassRateLimit
		case upstreamErr.Status == http.StatusUnauthorized || upstreamErr.Status == http.StatusForbidden:
			codeClass = upstreamClassAuth
		case upstreamErr.Class() == anthropic.ResponseClassRetryableError:
			codeClass = upstreamClassServerError
		case upstreamErr.Status >= 500:
			codeClass = upstreamClassServerError
		case upstreamErr.Status >= 400:
			codeClass = upstreamClassInvalidRequest
		}
		aerr := mapUpstreamForFamily(adapterRouteOpenAI, "anthropic", upstreamErr.Status, codeClass, "", message)
		aerr.Cause = err
		aerr.UpstreamStatus = upstreamErr.Status
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
	server *Server
}

func (d *collectResponseDispatcher) Log() *slog.Logger {
	return d.server.log
}

func (d *collectResponseDispatcher) AnthropicStreamClient() anthropicbackend.StreamClient {
	return d.server.anthr
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

func (d *collectResponseDispatcher) LogTerminal(ctx context.Context, ev adapterruntime.RequestEvent) {
	adapterruntime.LogTerminal(d.server.log, ctx, d.server.deps.RequestEvents, ev)
}

func (d *collectResponseDispatcher) CacheTTL() string {
	return d.server.cfg.ClientIdentity.PromptCacheTTL
}

type streamResponseDispatcher struct {
	server *Server
}

func (d *streamResponseDispatcher) Log() *slog.Logger {
	return d.server.log
}

func (d *streamResponseDispatcher) AnthropicStreamClient() anthropicbackend.StreamClient {
	return d.server.anthr
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

func (d *streamResponseDispatcher) LogTerminal(ctx context.Context, ev adapterruntime.RequestEvent) {
	adapterruntime.LogTerminal(d.server.log, ctx, d.server.deps.RequestEvents, ev)
}

func (d *streamResponseDispatcher) CacheTTL() string {
	return d.server.cfg.ClientIdentity.PromptCacheTTL
}
