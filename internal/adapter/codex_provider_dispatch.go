package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/gklog/correlation"
)

// codexEgressContext builds a derived context for a Codex provider
// Execute call. It:
//
//  1. Registers a parent egress session so the reload drain tracks
//     the overall websocket transport lifetime.
//  2. Injects a BeforeAttempt hook via codex.WithBeforeAttempt so
//     each retry attempt is registered as a nested child session.
//
// The caller must invoke the returned release func when Execute returns.
func (s *Server) codexEgressContext(
	ctx context.Context,
	requestID string,
	wsURL string,
) (context.Context, func(string)) {
	egressCtx, parentSess, releaseEgress := registerEgress(ctx, s.egressRegistry, egressSessionKindWebsocket, EgressMeta{
		Provider:          "codex",
		UpstreamURL:       wsURL,
		UpstreamRequestID: "",
		AttemptNo:         0,
		ParentRequestID:   requestID,
	})
	// BeforeAttempt hook: called by the transport's retry loop for each
	// attempt (1-based). Registers the attempt as a nested child of the
	// parent egress session so operators can correlate attempt spans.
	hook := func(attemptCtx context.Context, attemptNo int) (context.Context, func(string)) {
		childCtx, _, releaseAttempt := registerEgress(
			attemptCtx,
			s.egressRegistry,
			egressSessionKindAttempt,
			EgressMeta{
				Provider:          "codex",
				UpstreamURL:       wsURL,
				UpstreamRequestID: "",
				AttemptNo:         attemptNo,
				ParentRequestID:   requestID,
			},
			livetrack.WithParent(parentSess),
		)
		return childCtx, releaseAttempt
	}
	egressCtx = adaptercodex.WithBeforeAttempt(egressCtx, hook)
	return egressCtx, releaseEgress
}

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
	reqID string,
	ingressCtx ingresscontract.IngressContext,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	started := clock.Now()
	_ = ingressCtx // resolvedReq.Cursor carries the same value; keep parameter for future hooks.

	alias := resolvedRequestAlias(&resolvedReq)
	s.emitRequestStarted(r.Context(), &resolvedReq, "direct", reqID, alias, req.Stream)

	if req.Stream {
		s.dispatchCodexProviderStream(r.Context(), w, r, req, reqID, started, resolvedReq)
		return
	}
	s.dispatchCodexProviderCollect(r.Context(), w, r, req, reqID, started, resolvedReq)
}

func (s *Server) dispatchCodexProviderStream(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	reqID string,
	started time.Time,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	alias := resolvedRequestAlias(&resolvedReq)
	writer, err := newProviderStreamWriter(ctx, s, w, reqID, alias, "codex")
	if err != nil {
		s.respondAdapterError(w, r, adapterErrInternal(err.Error(), err))
		return
	}

	s.emitRequestStreamOpened(ctx, &resolvedReq, "direct", reqID, alias)

	// Register the overall Codex egress call and inject the per-attempt
	// hook before dispatching to the provider.
	egressCtx, releaseEgress := s.codexEgressContext(ctx, reqID, adaptercodex.WebsocketURL(""))
	defer releaseEgress("codex.stream.done")

	result, runErr := s.codexProvider.Execute(egressCtx, resolvedReq, writer)
	if runErr != nil {
		s.handleCodexProviderStreamError(ctx, w, r, writer, runErr, resolvedReq, reqID, started)
		return
	}
	corr := clydeingress.WithUpstreamResponseID(correlation.FromContext(ctx), result.UpstreamResponseID)
	ctx = correlation.WithContext(ctx, corr)
	usage := result.Usage
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(ctx, result.UsageNoticeWindows)
	}
	result.UsageNotices = notices
	if resolvedReq.ContextBudget.InputTokens > 0 {
		usage.MaxTokens = resolvedReq.ContextBudget.InputTokens
	}
	result.Usage = usage
	if err := writer.finalizeStream(ctx, result, req.StreamOptions != nil && req.StreamOptions.IncludeUsage); err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_finalize_error", slog.String("concern", "adapter.chat.render"), slog.String("backend", "codex"),
			slog.String("request_id", reqID),
			slog.String("alias", alias),
			slog.Any("err", err),
		)
		return
	}

	completedAttrs := codexCompletedAttrs(reqID, alias, usage, result, finishReason, clock.Since(started).Milliseconds(), true)
	completedAttrs = append(completedAttrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.completed", append([]slog.Attr{slog.String("concern", "adapter.chat.render")}, completedAttrs...)...)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:                      adapterruntime.RequestStageCompleted,
		Provider:                   "codex_direct",
		Backend:                    resolvedReq.Provider.String(),
		RequestID:                  reqID,
		Alias:                      alias,
		ModelID:                    alias,
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
		DurationMs:                 clock.Since(started).Milliseconds(),
		Correlation:                corr, Err: "",
	})
}

func (s *Server) handleCodexProviderStreamError(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	writer *providerStreamWriter,
	runErr error,
	resolvedReq adapterresolver.ResolvedRequest,
	reqID string,
	started time.Time,
) {
	alias := resolvedRequestAlias(&resolvedReq)
	aerr := codexProviderAdapterError(runErr)
	aerr.Backend = resolvedReq.Provider.String()
	aerr.ModelAlias = alias
	aerr.ResolvedModelName = resolvedReq.Model
	aerr.Cause = runErr
	s.logCodexProviderError(ctx, reqID, alias, aerr, writer.headersWritten)
	s.logCodexProviderTerminalFailure(ctx, resolvedReq, reqID, started, runErr, true)
	if writer.headersWritten {
		if err := writer.writeStreamError(ctx, aerr); err != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_error_write_failed", slog.String("concern", "adapter.chat.render"), slog.String("backend", "codex"),
				slog.String("request_id", reqID),
				slog.Any("err", err),
			)
		}
		return
	}
	s.respondAdapterError(w, r, aerr)
}

func (s *Server) dispatchCodexProviderCollect(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	_ ChatRequest,
	reqID string,
	started time.Time,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	alias := resolvedRequestAlias(&resolvedReq)
	collector := newProviderCollectorWriter()
	// Register the overall Codex egress call and inject the per-attempt
	// hook before dispatching to the provider.
	egressCtx, releaseEgress := s.codexEgressContext(ctx, reqID, adaptercodex.WebsocketURL(""))
	defer releaseEgress("codex.collect.done")

	result, runErr := s.codexProvider.Execute(egressCtx, resolvedReq, collector)
	if runErr != nil {
		aerr := codexProviderAdapterError(runErr)
		aerr.Backend = resolvedReq.Provider.String()
		aerr.ModelAlias = alias
		aerr.ResolvedModelName = resolvedReq.Model
		aerr.Cause = runErr
		s.logCodexProviderError(ctx, reqID, alias, aerr, false)
		s.logCodexProviderTerminalFailure(ctx, resolvedReq, reqID, started, runErr, false)
		s.respondAdapterError(w, r, aerr)
		return
	}
	corr := clydeingress.WithUpstreamResponseID(correlation.FromContext(ctx), result.UpstreamResponseID)
	ctx = correlation.WithContext(ctx, corr)
	finishReason := normalizedProviderFinishReason(result)
	var notices []adapterruntime.UsageNotice
	if finishReason == defaultProviderFinishReason {
		notices = s.evaluateUsageNotices(ctx, result.UsageNoticeWindows)
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
		HasSubagentToolCall:        result.HasSubagentToolCall, UsageTelemetry: adaptercodex.
						UsageTelemetry{UsagePresent: false, InputTokens: 0, OutputTokens: 0, TotalTokens: 0, InputTokensDetailsPresent: false, CachedTokens: 0, OutputTokensDetailsPresent: false, ReasoningOutputTokens: 0},

		ResponseID: "", OutputItems: nil,
	}
	mergedEvents := adapterruntime.EventsWithInjectedUsageNotices(ctx, collector.events, notices)
	merged := adaptercodex.MergeEvents(reqID, alias, systemFingerprint, mergedEvents, runResult)
	usage := result.Usage
	if resolvedReq.ContextBudget.InputTokens > 0 {
		usage.MaxTokens = resolvedReq.ContextBudget.InputTokens
	}
	if merged.Usage != nil {
		merged.Usage.MaxTokens = usage.MaxTokens
	}
	mergedBody, err := json.Marshal(merged)
	if err != nil {
		s.log.WarnContext(ctx, "adapter.codex.collect_marshal_failed", "concern", "adapter.providers.codex.request", "err", err)
		return
	}
	writeJSON(w, mergedBody)
	completedAttrs := codexCompletedAttrs(reqID, alias, usage, result, finishReason, clock.Since(started).Milliseconds(), false)
	completedAttrs = append(completedAttrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.completed", append([]slog.Attr{slog.String("concern", "adapter.chat.render")}, completedAttrs...)...)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:                      adapterruntime.RequestStageCompleted,
		Provider:                   "codex_direct",
		Backend:                    resolvedReq.Provider.String(),
		RequestID:                  reqID,
		Alias:                      alias,
		ModelID:                    alias,
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
		DurationMs:                 clock.Since(started).Milliseconds(),
		Correlation:                corr, Err: "",
	})
}

func (s *Server) logCodexProviderError(ctx context.Context, reqID, alias string, aerr *adapterError, streamHeadersWritten bool) {
	s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.codex.provider_error_mapped", slog.String("concern", "adapter.providers.codex.errors"), slog.String("request_id", reqID),
		slog.String("alias", alias),
		slog.Int("status", aerr.HTTPStatus),
		slog.String("error_class", string(aerr.Class)),
		slog.String("error_code", aerr.Code),
		slog.String("error_param", aerr.Param),
		slog.Bool("stream_headers_written", streamHeadersWritten),
	)
}

func (s *Server) logCodexProviderTerminalFailure(
	ctx context.Context,
	resolvedReq adapterresolver.ResolvedRequest,
	reqID string,
	started time.Time,
	runErr error,
	stream bool,
) {
	alias := resolvedRequestAlias(&resolvedReq)
	adapterruntime.LogTerminal(s.log, ctx, s.deps.RequestEvents, adapterruntime.RequestEvent{
		Stage:      adapterruntime.RequestStageFailed,
		Provider:   "codex_direct",
		Backend:    resolvedReq.Provider.String(),
		RequestID:  reqID,
		Alias:      alias,
		ModelID:    alias,
		Stream:     stream,
		DurationMs: clock.Since(started).Milliseconds(),
		Err:        runErr.Error(), FinishReason:

		// codexProviderAdapterError maps a codex provider error to the
		// Cursor-safe adapterError shape. Codex must never surface
		// HTTP 5xx + server_error to Cursor; all non-2xx returns flow through
		// mapUpstreamForFamily. Schema violations are detected by message
		// pattern (missing_required_parameter / [ObjectParam] / unknown
		// parameter) so the operator sees a meaningful diagnostic instead of
		// the generic upstream_failed.
		"", TokensIn: 0, TokensOut: 0, CacheReadTokens: 0, CacheCreationTokens: 0, DerivedCacheCreationTokens: 0, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false, Correlation: correlation.
				Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	})
}

func codexProviderAdapterError(err error) *adapterError {
	var contextErr *adaptercodex.ContextWindowError
	if errors.As(err, &contextErr) {
		aerr := newAdapterError(adapterErrorContextLengthExceeded, "This model's maximum context length was exceeded. Please reduce the length of the messages.")
		aerr.Provider = "codex"
		aerr.Cause = err
		return aerr
	}
	var unsupportedModelErr *adaptercodex.UnsupportedModelError
	if errors.As(err, &unsupportedModelErr) {
		aerr := newAdapterError(adapterErrorModelNotSupported, unsupportedModelErr.Error())
		aerr.Provider = "codex"
		aerr.Cause = err
		return aerr
	}
	// An upstream non-2xx carries its body snippet on a typed error whose
	// Error() is snippet-free for logs; fold the snippet into the client
	// message here, the one place that builds the Cursor-facing envelope.
	message := err.Error()
	var upstreamStatusErr *adaptercodex.UpstreamStatusError
	if errors.As(err, &upstreamStatusErr) {
		message = upstreamStatusErr.ClientMessage()
	}
	codeClass := codexClassifyError(message)
	aerr := mapUpstreamForFamily(adapterRouteOpenAI, "codex", 0, codeClass, "", message)
	aerr.Cause = err
	return aerr
}

// codexClassifyError detects schema violations and other recognizable
// classes from the upstream message pattern. Codex emits messages
// like "[ObjectParam] [input[5].summary] [missing_required_parameter]"
// when the adapter forwards a malformed Reasoning item; classifying
// these explicitly so the chat transcript shows
// upstream_malformed_request instead of the generic upstream_failed.
func codexClassifyError(message string) upstreamCodeClass {
	lowered := strings.ToLower(message)
	if strings.Contains(lowered, "missing_required_parameter") ||
		strings.Contains(lowered, "[objectparam]") ||
		strings.Contains(lowered, "unknown parameter") {
		return upstreamClassSchemaViolation
	}
	return upstreamClassServerError
}
