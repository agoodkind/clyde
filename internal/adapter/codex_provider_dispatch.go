package adapter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/correlation"
)

// codexCompletedAttrs builds the structured attribute slice for the
// adapter.chat.completed emit on the Codex dispatch path. Both the
// streaming and non-streaming finalize sites share this builder so the
// emit shape stays stable. Codex never reports cache_creation per the
// upstream contract, so cache_creation_reported is hard-coded to false
// here. See research/codex/codex-rs/codex-api/src/sse/responses.rs.
func codexCompletedAttrs(reqID, alias string, usage adapteropenai.Usage, result adapterprovider.Result, finishReason string, durationMs int64, stream bool) []slog.Attr {
	return []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("model", alias),
		slog.Int("prompt_tokens", usage.PromptTokens),
		slog.Int("completion_tokens", usage.CompletionTokens),
		slog.Int("cache_read_tokens", usage.CachedTokens()),
		slog.Int("cache_creation_tokens", 0),
		slog.Bool("cache_creation_reported", false),
		slog.Int("derived_cache_creation_tokens", result.DerivedCacheCreationTokens),
		slog.Int64("duration_ms", durationMs),
		slog.Bool("stream", stream),
		slog.String("backend", "codex"),
		slog.String("provider_path", "provider"),
		slog.String("finish_reason", finishReason),
		slog.Bool("reasoning_signaled", result.ReasoningSignaled),
		slog.Bool("reasoning_visible", result.ReasoningVisible),
		slog.Int("tool_call_count", result.ToolCallCount),
		slog.Any("tool_call_names", result.ToolCallNames),
		slog.Bool("has_subagent_tool_call", result.HasSubagentToolCall),
	}
}

// dispatchCodexProvider routes a Codex-bound request through the new
// codex.Provider via the Server's provider.Registry. It is invoked
// by dispatchResolvedChat when the resolver successfully maps the
// request to ProviderCodex and a Codex provider is registered.
//
// Streaming and non-streaming share the same Provider.Execute call;
// the writer choice is what differs. The streaming writer forwards
// chunks over SSE in real time. The non-streaming writer buffers
// chunks and the merged ChatResponse is written once Execute
// returns.
func (s *Server) dispatchCodexProvider(
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	model ResolvedModel,
	reqID string,
	cursorReq adaptercursor.Request,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	started := adapterClock.Now()
	_ = cursorReq // resolvedReq.Cursor carries the same value; keep parameter for future hooks.

	s.emitRequestStarted(r.Context(), model, "direct", reqID, model.Alias, req.Stream)

	if req.Stream {
		s.dispatchCodexProviderStream(r.Context(), w, r, req, model, reqID, started, resolvedReq)
		return
	}
	s.dispatchCodexProviderCollect(r.Context(), w, r, req, model, reqID, started, resolvedReq)
}

func (s *Server) dispatchCodexProviderStream(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	model ResolvedModel,
	reqID string,
	started time.Time,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	writer, err := newProviderStreamWriter(ctx, s, w, reqID, model.Alias, "codex")
	if err != nil {
		s.respondAdapterError(w, r, adapterErrInternal(err.Error(), err))
		return
	}

	s.emitRequestStreamOpened(r.Context(), model, "direct", reqID, model.Alias, true)

	result, runErr := s.codexProvider.Execute(ctx, resolvedReq, writer)
	if runErr != nil {
		status, errorBody := codexProviderErrorResponse(runErr)
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.codex.provider_error_mapped",
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.Int("status", status),
			slog.String("error_type", errorBody.Type),
			slog.String("error_code", errorBody.Code),
			slog.String("error_param", errorBody.Param),
			slog.Bool("stream_headers_written", writer.headersWritten),
		)
		adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
			Stage:      adapterruntime.RequestStageFailed,
			Provider:   "codex_direct",
			Backend:    model.Backend,
			RequestID:  reqID,
			Alias:      model.Alias,
			ModelID:    model.Alias,
			Stream:     true,
			DurationMs: time.Since(started).Milliseconds(),
			Err:        runErr.Error(),
		})
		if writer.headersWritten {
			if err := writer.writeStreamErrorBody(ctx, errorBody); err != nil {
				s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_error_write_failed",
					slog.String("backend", "codex"),
					slog.String("request_id", reqID),
					slog.Any("err", err),
				)
			}
			return
		}
		mapped := adapterErrFromOpenAI(status, errorBody)
		mapped.Provider = "codex"
		mapped.Backend = model.Backend
		mapped.ModelAlias = model.Alias
		mapped.ResolvedModel = model.ClaudeModel
		mapped.Cause = runErr
		s.respondAdapterError(w, r, mapped)
		return
	}
	corr := correlation.FromContext(ctx).WithUpstreamResponseID(result.UpstreamResponseID)
	ctx = correlation.WithContext(ctx, corr)
	usage := result.Usage
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(result.UsageNoticeWindows)
	}
	result.UsageNotices = notices
	if model.Context > 0 {
		usage.MaxTokens = model.Context
	}
	result.Usage = usage
	if err := writer.finalizeStream(ctx, result, req.StreamOptions != nil && req.StreamOptions.IncludeUsage); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_finalize_error",
			slog.String("backend", "codex"),
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.Any("err", err),
		)
		return
	}

	completedAttrs := codexCompletedAttrs(reqID, model.Alias, usage, result, finishReason, time.Since(started).Milliseconds(), true)
	completedAttrs = append(completedAttrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.completed", completedAttrs...)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:                      adapterruntime.RequestStageCompleted,
		Provider:                   "codex_direct",
		Backend:                    model.Backend,
		RequestID:                  reqID,
		Alias:                      model.Alias,
		ModelID:                    model.Alias,
		Stream:                     true,
		FinishReason:               finishReason,
		TokensIn:                   usage.PromptTokens,
		TokensOut:                  usage.CompletionTokens,
		CacheReadTokens:            usage.CachedTokens(),
		CacheCreationTokens:        0,
		DerivedCacheCreationTokens: result.DerivedCacheCreationTokens,
		ToolCallCount:              result.ToolCallCount,
		ToolCallNames:              result.ToolCallNames,
		HasSubagentToolCall:        result.HasSubagentToolCall,
		DurationMs:                 time.Since(started).Milliseconds(),
		Correlation:                corr,
	})
}

func (s *Server) dispatchCodexProviderCollect(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	_ ChatRequest,
	model ResolvedModel,
	reqID string,
	started time.Time,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	collector := newProviderCollectorWriter()
	result, runErr := s.codexProvider.Execute(ctx, resolvedReq, collector)
	if runErr != nil {
		status, errorBody := codexProviderErrorResponse(runErr)
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.codex.provider_error_mapped",
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.Int("status", status),
			slog.String("error_type", errorBody.Type),
			slog.String("error_code", errorBody.Code),
			slog.String("error_param", errorBody.Param),
			slog.Bool("stream_headers_written", false),
		)
		adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
			Stage:      adapterruntime.RequestStageFailed,
			Provider:   "codex_direct",
			Backend:    model.Backend,
			RequestID:  reqID,
			Alias:      model.Alias,
			ModelID:    model.Alias,
			Stream:     false,
			DurationMs: time.Since(started).Milliseconds(),
			Err:        runErr.Error(),
		})
		mapped := adapterErrFromOpenAI(status, errorBody)
		mapped.Provider = "codex"
		mapped.Backend = model.Backend
		mapped.ModelAlias = model.Alias
		mapped.ResolvedModel = model.ClaudeModel
		mapped.Cause = runErr
		s.respondAdapterError(w, r, mapped)
		return
	}
	corr := correlation.FromContext(ctx).WithUpstreamResponseID(result.UpstreamResponseID)
	ctx = correlation.WithContext(ctx, corr)
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(result.UsageNoticeWindows)
	}
	result.UsageNotices = notices
	runResult := adaptercodex.RunResult{
		Usage:                      result.Usage,
		FinishReason:               finishReason,
		ReasoningSignaled:          result.ReasoningSignaled,
		ReasoningVisible:           result.ReasoningVisible,
		DerivedCacheCreationTokens: result.DerivedCacheCreationTokens,
		ToolCallCount:              result.ToolCallCount,
		ToolCallNames:              result.ToolCallNames,
		HasSubagentToolCall:        result.HasSubagentToolCall,
	}
	mergedEvents := adapterruntime.EventsWithInjectedUsageNotices(ctx, collector.events, notices)
	merged := adaptercodex.MergeEvents(reqID, model.Alias, systemFingerprint, mergedEvents, runResult)
	usage := result.Usage
	if model.Context > 0 {
		usage.MaxTokens = model.Context
	}
	if merged.Usage != nil {
		merged.Usage.MaxTokens = usage.MaxTokens
	}
	writeJSON(w, http.StatusOK, merged)
	completedAttrs := codexCompletedAttrs(reqID, model.Alias, usage, result, finishReason, time.Since(started).Milliseconds(), false)
	completedAttrs = append(completedAttrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.completed", completedAttrs...)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:                      adapterruntime.RequestStageCompleted,
		Provider:                   "codex_direct",
		Backend:                    model.Backend,
		RequestID:                  reqID,
		Alias:                      model.Alias,
		ModelID:                    model.Alias,
		Stream:                     false,
		FinishReason:               finishReason,
		TokensIn:                   usage.PromptTokens,
		TokensOut:                  usage.CompletionTokens,
		CacheReadTokens:            usage.CachedTokens(),
		CacheCreationTokens:        0,
		DerivedCacheCreationTokens: result.DerivedCacheCreationTokens,
		ToolCallCount:              result.ToolCallCount,
		ToolCallNames:              result.ToolCallNames,
		HasSubagentToolCall:        result.HasSubagentToolCall,
		DurationMs:                 time.Since(started).Milliseconds(),
		Correlation:                corr,
	})
}

func codexProviderErrorResponse(err error) (int, ErrorBody) {
	var contextErr *adaptercodex.ContextWindowError
	if errors.As(err, &contextErr) {
		return http.StatusBadRequest, ErrorBody{
			Message: "This model's maximum context length was exceeded. Please reduce the length of the messages.",
			Type:    "invalid_request_error",
			Code:    "context_length_exceeded",
			Param:   "messages",
		}
	}
	var unsupportedModelErr *adaptercodex.UnsupportedModelError
	if errors.As(err, &unsupportedModelErr) {
		return http.StatusBadRequest, ErrorBody{
			Message: unsupportedModelErr.Error(),
			Type:    "invalid_request_error",
			Code:    "model_not_supported",
			Param:   "model",
		}
	}
	return http.StatusBadGateway, ErrorBody{
		Message: err.Error(),
		Type:    "server_error",
		Code:    "upstream_failed",
	}
}
