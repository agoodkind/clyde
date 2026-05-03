package anthropicbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

type TrackedUsage struct {
	Usage      adapteropenai.Usage
	RawPrompt  int
	RawTotal   int
	RolledFrom int
}

type ResponseSSEWriter interface {
	WriteSSEHeaders()
	EmitStreamChunk(string, adapteropenai.StreamChunk) error
	EmitStreamError(adapteropenai.ErrorBody) error
	WriteStreamDone() error
	HasCommittedHeaders() bool
}

type ResponseDispatcher interface {
	Log() *slog.Logger
	EmitRequestStarted(context.Context, adaptermodel.ResolvedModel, string, string, string, bool)
	EmitRequestStreamOpened(context.Context, adaptermodel.ResolvedModel, string, string, string, bool)
	NewAnthropicSSEWriter(http.ResponseWriter) (ResponseSSEWriter, error)
	AnthropicStreamClient() StreamClient
	SystemFingerprint() string
	StreamChunkHasVisibleContent(adapteropenai.StreamChunk) bool
	WriteEvent(adapterrender.Event) error
	FlushEventWriter() error
	CollectedEvents() []adapterrender.Event
	TrackAnthropicContextUsage(string, adapteropenai.Usage) TrackedUsage
	WriteJSON(http.ResponseWriter, int, adapteropenai.ChatResponse)
	WriteErrorJSON(http.ResponseWriter, int, adapteropenai.ErrorResponse)
	LogTerminal(context.Context, adapterruntime.RequestEvent)
	LogCacheUsageAnthropic(context.Context, string, string, string, anthropic.Usage)
	CacheTTL() string
	NoticesEnabled() bool
	NoticeUsageThresholdsUsedPercent() []float64
	ClaimNotice(string, time.Time) bool
	UnclaimNotice(string, time.Time)
}

type ExecutionRuntime interface {
	Log() *slog.Logger
	AnthropicStreamClient() StreamClient
	TrackAnthropicContextUsage(string, adapteropenai.Usage) TrackedUsage
	LogTerminal(context.Context, adapterruntime.RequestEvent)
	LogCacheUsageAnthropic(context.Context, string, string, string, anthropic.Usage)
	CacheTTL() string
}

type CollectExecutionResult struct {
	Events              []adapterrender.Event
	Usage               adapteropenai.Usage
	FinishReason        string
	AnthropicStopReason string
	AnthropicUsage      anthropic.Usage
}

type StreamExecutionResult struct {
	Usage               adapteropenai.Usage
	FinishReason        string
	AnthropicUsage      anthropic.Usage
	EmittedContent      bool
	ToolCallCount       int
	ToolCallNames       []string
	HasSubagentToolCall bool
}

func RunCollectExecution(
	rt ExecutionRuntime,
	ctx context.Context,
	req anthropic.Request,
	model adaptermodel.ResolvedModel,
	reqID string,
	started time.Time,
	trackerKey string,
	emit func(adapterrender.Event) error,
	flush func() error,
	collectedEvents func() []adapterrender.Event,
) (CollectExecutionResult, error) {
	anthUsage, anthStopReason, finishReason, err := RunTranslatorEvents(rt.AnthropicStreamClient(), ctx, req, model, reqID, emit)
	if err != nil {
		return CollectExecutionResult{}, err
	}
	if err := flush(); err != nil {
		return CollectExecutionResult{}, err
	}
	rawUsage := usageWithContextWindow(UsageFromAnthropic(anthUsage), model.Context)
	tracked := rt.TrackAnthropicContextUsage(trackerKey, rawUsage)
	u := tracked.Usage
	if u.PromptTokens != rawUsage.PromptTokens || u.TotalTokens != rawUsage.TotalTokens {
		rt.Log().LogAttrs(ctx, slog.LevelInfo, "adapter.context_usage.tracked",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.Int("raw_prompt_tokens", tracked.RawPrompt),
			slog.Int("raw_total_tokens", tracked.RawTotal),
			slog.Int("rolled_output_tokens", tracked.RolledFrom),
			slog.Int("surfaced_prompt_tokens", u.PromptTokens),
			slog.Int("surfaced_total_tokens", u.TotalTokens),
		)
	}
	rt.LogCacheUsageAnthropic(ctx, "anthropic", reqID, model.Alias, anthUsage)
	adapterruntime.LogCompleted(rt.Log(), ctx, adapterruntime.CompletedAttrs{
		Backend:             "anthropic",
		Provider:            "anthropic-oauth",
		Path:                "oauth",
		SessionID:           reqID,
		RequestID:           reqID,
		Alias:               model.Alias,
		ModelID:             req.Model,
		FinishReason:        finishReason,
		TokensIn:            u.PromptTokens,
		TokensOut:           u.CompletionTokens,
		CacheReadTokens:     anthUsage.CacheReadInputTokens,
		CacheCreationTokens: anthUsage.CacheCreationInputTokens,
		CacheTTL:            rt.CacheTTL(),
		DurationMs:          time.Since(started).Milliseconds(),
		Stream:              false,
	})
	breakdown := adapterruntime.EstimateCost(adapterruntime.CostInputs{
		ModelID:             req.Model,
		TTL:                 rt.CacheTTL(),
		InputTokens:         u.PromptTokens,
		OutputTokens:        u.CompletionTokens,
		CacheCreationTokens: anthUsage.CacheCreationInputTokens,
		CacheReadTokens:     anthUsage.CacheReadInputTokens,
	})
	rt.LogTerminal(ctx, adapterruntime.RequestEvent{
		Stage:               adapterruntime.RequestStageCompleted,
		Provider:            "anthropic-oauth",
		Backend:             model.Backend,
		RequestID:           reqID,
		Alias:               model.Alias,
		ModelID:             req.Model,
		Stream:              false,
		FinishReason:        finishReason,
		TokensIn:            u.PromptTokens,
		TokensOut:           u.CompletionTokens,
		CacheReadTokens:     anthUsage.CacheReadInputTokens,
		CacheCreationTokens: anthUsage.CacheCreationInputTokens,
		CostMicrocents:      breakdown.TotalMicrocents,
		DurationMs:          time.Since(started).Milliseconds(),
	})
	var events []adapterrender.Event
	if collectedEvents != nil {
		events = collectedEvents()
	}
	return CollectExecutionResult{
		Events:              events,
		Usage:               u,
		FinishReason:        finishReason,
		AnthropicStopReason: anthStopReason,
		AnthropicUsage:      anthUsage,
	}, nil
}

func RunStreamExecution(
	rt ExecutionRuntime,
	ctx context.Context,
	req anthropic.Request,
	model adaptermodel.ResolvedModel,
	reqID string,
	trackerKey string,
	emit func(adapterrender.Event) error,
) (StreamExecutionResult, error) {
	emittedContent := false
	toolCallCount := 0
	var toolCallNames []string
	hasSubagentToolCall := false
	emitTracked := func(ev adapterrender.Event) error {
		if eventHasVisibleContent(ev) {
			emittedContent = true
		}
		count, names, hasSubagent := toolCallStats(ev)
		toolCallCount += count
		appendUniqueStrings(&toolCallNames, names)
		hasSubagentToolCall = hasSubagentToolCall || hasSubagent
		return emit(ev)
	}
	anthUsage, _, finishReason, err := RunTranslatorEvents(rt.AnthropicStreamClient(), ctx, req, model, reqID, emitTracked)
	rawFinalUsage := usageWithContextWindow(UsageFromAnthropic(anthUsage), model.Context)
	tracked := rt.TrackAnthropicContextUsage(trackerKey, rawFinalUsage)
	finalUsage := tracked.Usage
	if finalUsage.PromptTokens != rawFinalUsage.PromptTokens || finalUsage.TotalTokens != rawFinalUsage.TotalTokens {
		rt.Log().LogAttrs(ctx, slog.LevelInfo, "adapter.context_usage.tracked",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.Int("raw_prompt_tokens", tracked.RawPrompt),
			slog.Int("raw_total_tokens", tracked.RawTotal),
			slog.Int("rolled_output_tokens", tracked.RolledFrom),
			slog.Int("surfaced_prompt_tokens", finalUsage.PromptTokens),
			slog.Int("surfaced_total_tokens", finalUsage.TotalTokens),
		)
	}
	return StreamExecutionResult{
		Usage:               finalUsage,
		FinishReason:        finishReason,
		AnthropicUsage:      anthUsage,
		EmittedContent:      emittedContent,
		ToolCallCount:       toolCallCount,
		ToolCallNames:       toolCallNames,
		HasSubagentToolCall: hasSubagentToolCall,
	}, err
}

func CollectResponse(
	d ResponseDispatcher,
	w http.ResponseWriter,
	ctx context.Context,
	req anthropic.Request,
	model adaptermodel.ResolvedModel,
	reqID string,
	started time.Time,
	jsonCoercion anthropic.JSONCoercion,
	escalate bool,
	trackerKey string,
) error {
	d.EmitRequestStarted(ctx, model, "oauth", reqID, req.Model, false)
	var notice *anthropic.Notice
	previousOnHeaders := req.OnHeaders
	req.OnHeaders = func(h http.Header) {
		if previousOnHeaders != nil {
			previousOnHeaders(h)
		}
		notice = adapterruntime.EvaluateNoticeFromHeaders(
			h,
			d.NoticesEnabled(),
			d.NoticeUsageThresholdsUsedPercent(),
			d.ClaimNotice,
		)
	}
	emit := func(ev adapterrender.Event) error {
		return d.WriteEvent(ev)
	}
	result, err := RunCollectExecution(
		d,
		ctx,
		req,
		model,
		reqID,
		started,
		trackerKey,
		emit,
		d.FlushEventWriter,
		d.CollectedEvents,
	)
	if err != nil {
		adapterruntime.LogFailed(d.Log(), ctx, adapterruntime.FailedAttrs{
			Backend:    "anthropic",
			Provider:   "anthropic-oauth",
			RequestID:  reqID,
			Alias:      model.Alias,
			ModelID:    req.Model,
			Err:        err,
			DurationMs: time.Since(started).Milliseconds(),
		})
		d.LogTerminal(ctx, adapterruntime.RequestEvent{
			Stage:      adapterruntime.RequestStageFailed,
			Provider:   "anthropic-oauth",
			Backend:    model.Backend,
			RequestID:  reqID,
			Alias:      model.Alias,
			ModelID:    req.Model,
			Stream:     false,
			DurationMs: time.Since(started).Milliseconds(),
			Err:        err.Error(),
		})
		errMsg := err.Error()
		if notice != nil {
			if escalate {
				d.UnclaimNotice(notice.Kind, notice.ResetsAt)
				d.Log().LogAttrs(ctx, slog.LevelDebug, "adapter.notice.unclaimed_on_escalate",
					slog.String("subcomponent", "adapter"),
					slog.String("request_id", reqID),
					slog.String("alias", model.Alias),
					slog.String("kind", notice.Kind),
				)
			} else {
				errMsg = errMsg + " · " + notice.Text
				d.Log().LogAttrs(ctx, slog.LevelInfo, "adapter.notice.injected_into_error",
					slog.String("subcomponent", "adapter"),
					slog.String("request_id", reqID),
					slog.String("alias", model.Alias),
					slog.String("kind", notice.Kind),
					slog.String("notice_text", notice.Text),
				)
			}
		}
		return adapterruntime.EscalateOrWrite(
			err,
			escalate,
			func(status int, code, msg string) error {
				d.WriteErrorJSON(w, status, adapteropenai.ErrorResponse{
					Error: adapteropenai.ErrorBody{Message: msg, Type: "server_error", Code: code},
				})
				return nil
			},
			http.StatusBadGateway,
			"upstream_failed",
			errMsg,
		)
	}
	resp := MergeCollectedEvents(
		reqID,
		model.Alias,
		d.SystemFingerprint(),
		result.Events,
		result.Usage,
		result.FinishReason,
		backendJSONCoercion(jsonCoercion),
		result.AnthropicStopReason,
	)
	resp, _ = adapterruntime.NoticeForResponseHeaders(resp, notice, func(kind string, resetsAt time.Time) {
		d.UnclaimNotice(kind, resetsAt)
	}, json.Marshal)
	d.WriteJSON(w, http.StatusOK, resp)
	return nil
}

func StreamResponse(
	d ResponseDispatcher,
	w http.ResponseWriter,
	r *http.Request,
	req anthropic.Request,
	model adaptermodel.ResolvedModel,
	reqID string,
	started time.Time,
	escalate bool,
	includeUsage bool,
	trackerKey string,
) error {
	d.EmitRequestStarted(r.Context(), model, "oauth", reqID, req.Model, true)
	sw, err := d.NewAnthropicSSEWriter(w)
	if err != nil {
		return adapterruntime.EscalateOrWrite(
			fmt.Errorf("no_flusher: streaming not supported by transport"),
			escalate,
			func(status int, code, msg string) error {
				d.WriteErrorJSON(w, status, adapteropenai.ErrorResponse{
					Error: adapteropenai.ErrorBody{Message: msg, Type: code},
				})
				return nil
			},
			http.StatusInternalServerError,
			"no_flusher",
			err.Error(),
		)
	}
	sw.WriteSSEHeaders()
	d.EmitRequestStreamOpened(r.Context(), model, "oauth", reqID, req.Model, true)

	emittedContent := false
	emitChunk := func(chunk adapteropenai.StreamChunk) error {
		if d.StreamChunkHasVisibleContent(chunk) {
			emittedContent = true
		}
		return sw.EmitStreamChunk(d.SystemFingerprint(), chunk)
	}
	emitEvent := func(ev adapterrender.Event) error {
		return d.WriteEvent(ev)
	}
	previousOnHeaders := req.OnHeaders
	req.OnHeaders = func(h http.Header) {
		if previousOnHeaders != nil {
			previousOnHeaders(h)
		}
		notice, err := adapterruntime.NoticeForStreamHeaders(
			reqID,
			model.Alias,
			h,
			d.NoticesEnabled(),
			d.NoticeUsageThresholdsUsedPercent(),
			func(chunk adapteropenai.StreamChunk) error {
				return emitChunk(chunk)
			},
			d.ClaimNotice,
			d.UnclaimNotice,
		)
		if err != nil && notice != nil {
			d.Log().LogAttrs(r.Context(), slog.LevelWarn, "adapter.notice.stream_emit_failed",
				slog.String("request_id", reqID),
				slog.String("alias", model.Alias),
				slog.String("model", req.Model),
				slog.String("kind", notice.Kind),
			)
		}
	}
	result, err := RunStreamExecution(d, r.Context(), req, model, reqID, trackerKey, emitEvent)
	anthUsage := result.AnthropicUsage
	finishReason := result.FinishReason
	emittedContent = result.EmittedContent
	if err != nil {
		d.Log().LogAttrs(r.Context(), slog.LevelWarn, "adapter.chat.stream_error",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.String("model", req.Model),
			slog.Any("err", err),
		)
		if escalate && !sw.HasCommittedHeaders() {
			return err
		}
		if !emittedContent {
			// When the upstream surfaces a typed
			// *anthropic.UpstreamError, emit a native OpenAI error
			// envelope (data: {"error":{...}}) so Cursor and other
			// OpenAI clients see a structured error rather than an
			// assistant-shaped chat message. Untyped errors keep the
			// actionable assistant text path.
			if ue, ok := anthropic.AsUpstreamError(err); ok {
				_ = sw.EmitStreamError(buildErrorBodyForUpstream(ue))
			} else {
				_ = EmitActionableStreamError(emitChunk, reqID, model.Alias, err)
			}
			finishReason = "stop"
		}
		d.LogTerminal(r.Context(), adapterruntime.RequestEvent{
			Stage:      adapterruntime.RequestStageFailed,
			Provider:   "anthropic-oauth",
			Backend:    model.Backend,
			RequestID:  reqID,
			Alias:      model.Alias,
			ModelID:    req.Model,
			Stream:     true,
			DurationMs: time.Since(started).Milliseconds(),
			Err:        err.Error(),
		})
	}
	if err == nil && !emittedContent && finishReason == "" && anthUsage.InputTokens == 0 && anthUsage.OutputTokens == 0 && anthUsage.CacheReadInputTokens == 0 && anthUsage.CacheCreationInputTokens == 0 {
		streamErr := fmt.Errorf("anthropic stream ended without content; claude authentication may be invalid")
		d.Log().LogAttrs(r.Context(), slog.LevelWarn, "adapter.chat.stream_empty",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.String("model", req.Model),
			slog.Any("err", streamErr),
		)
		_ = emitChunk(adapteropenai.StreamChunk{
			ID:      reqID,
			Object:  "chat.completion.chunk",
			Created: backendClock.Now().Unix(),
			Model:   model.Alias,
			Choices: []adapteropenai.StreamChoice{{
				Index: 0,
				Delta: adapteropenai.StreamDelta{
					Role:    "assistant",
					Content: "Clyde adapter upstream stream ended before producing content. Claude authentication may be invalid. Run `claude /login`, then retry.",
				},
			}},
		})
		finishReason = "stop"
	}
	_ = d.FlushEventWriter()
	finalUsage := result.Usage
	finishChunk := adapteropenai.StreamChunk{
		ID:      reqID,
		Object:  "chat.completion.chunk",
		Created: backendClock.Now().Unix(),
		Model:   model.Alias,
		Choices: []adapteropenai.StreamChoice{{Index: 0, Delta: adapteropenai.StreamDelta{}, FinishReason: &finishReason}},
	}
	if includeUsage {
		finishChunk.Usage = &finalUsage
	}
	_ = sw.EmitStreamChunk(d.SystemFingerprint(), finishChunk)
	if err == nil && emittedContent && finalUsage.PromptTokens == 0 && finalUsage.CompletionTokens == 0 {
		d.Log().LogAttrs(r.Context(), slog.LevelWarn, "adapter.anthropic.usage_missing",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.String("model", req.Model),
			slog.String("stop_reason", finishReason),
			slog.Int("raw_input_tokens", anthUsage.InputTokens),
			slog.Int("raw_output_tokens", anthUsage.OutputTokens),
			slog.Int("raw_cache_read_tokens", anthUsage.CacheReadInputTokens),
			slog.Int("raw_cache_creation_tokens", anthUsage.CacheCreationInputTokens),
		)
	}
	if includeUsage {
		_ = sw.EmitStreamChunk(d.SystemFingerprint(), adapteropenai.StreamChunk{
			ID:      reqID,
			Object:  "chat.completion.chunk",
			Created: backendClock.Now().Unix(),
			Model:   model.Alias,
			Choices: []adapteropenai.StreamChoice{},
			Usage:   &finalUsage,
		})
	}
	_ = sw.WriteStreamDone()

	d.LogCacheUsageAnthropic(r.Context(), "anthropic", reqID, model.Alias, anthUsage)
	adapterruntime.LogCompleted(d.Log(), r.Context(), adapterruntime.CompletedAttrs{
		Backend:             "anthropic",
		Provider:            "anthropic-oauth",
		Path:                "oauth",
		SessionID:           reqID,
		RequestID:           reqID,
		Alias:               model.Alias,
		ModelID:             req.Model,
		FinishReason:        finishReason,
		TokensIn:            finalUsage.PromptTokens,
		TokensOut:           finalUsage.CompletionTokens,
		CacheReadTokens:     anthUsage.CacheReadInputTokens,
		CacheCreationTokens: anthUsage.CacheCreationInputTokens,
		CacheTTL:            d.CacheTTL(),
		DurationMs:          time.Since(started).Milliseconds(),
		Stream:              true,
		ToolCallCount:       result.ToolCallCount,
		ToolCallNames:       result.ToolCallNames,
		HasSubagentToolCall: result.HasSubagentToolCall,
	})
	breakdown := adapterruntime.EstimateCost(adapterruntime.CostInputs{
		ModelID:             req.Model,
		TTL:                 d.CacheTTL(),
		InputTokens:         finalUsage.PromptTokens,
		OutputTokens:        finalUsage.CompletionTokens,
		CacheCreationTokens: anthUsage.CacheCreationInputTokens,
		CacheReadTokens:     anthUsage.CacheReadInputTokens,
	})
	if err == nil {
		d.LogTerminal(r.Context(), adapterruntime.RequestEvent{
			Stage:               adapterruntime.RequestStageCompleted,
			Provider:            "anthropic-oauth",
			Backend:             model.Backend,
			RequestID:           reqID,
			Alias:               model.Alias,
			ModelID:             req.Model,
			Stream:              true,
			FinishReason:        finishReason,
			TokensIn:            finalUsage.PromptTokens,
			TokensOut:           finalUsage.CompletionTokens,
			CacheReadTokens:     anthUsage.CacheReadInputTokens,
			CacheCreationTokens: anthUsage.CacheCreationInputTokens,
			CostMicrocents:      breakdown.TotalMicrocents,
			ToolCallCount:       result.ToolCallCount,
			ToolCallNames:       result.ToolCallNames,
			HasSubagentToolCall: result.HasSubagentToolCall,
			DurationMs:          time.Since(started).Milliseconds(),
		})
	}
	return nil
}

func eventHasVisibleContent(ev adapterrender.Event) bool {
	switch ev.Kind {
	case adapterrender.EventAssistantTextDelta, adapterrender.EventAssistantRefusalDelta, adapterrender.EventReasoningDelta:
		return strings.TrimSpace(ev.Text) != "" || ev.Text != ""
	case adapterrender.EventToolCallDelta:
		return len(ev.ToolCalls) > 0
	default:
		return false
	}
}

func toolCallStats(ev adapterrender.Event) (count int, names []string, hasSubagent bool) {
	if ev.Kind != adapterrender.EventToolCallDelta {
		return 0, nil, false
	}
	for _, tc := range ev.ToolCalls {
		name := strings.TrimSpace(tc.Function.Name)
		if strings.TrimSpace(tc.ID) == "" && name == "" {
			continue
		}
		count++
		appendUniqueStrings(&names, []string{name})
		switch name {
		case "Subagent", "Task", "spawn_agent":
			hasSubagent = true
		}
	}
	return count, names, hasSubagent
}

func appendUniqueStrings(dst *[]string, values []string) {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		seen := false
		for _, existing := range *dst {
			if existing == value {
				seen = true
				break
			}
		}
		if !seen {
			*dst = append(*dst, value)
		}
	}
}

func backendJSONCoercion(coercion anthropic.JSONCoercion) JSONCoercion {
	return JSONCoercion{
		Coerce:   coercion.Coerce,
		Validate: coercion.Validate,
	}
}

func usageWithContextWindow(u adapteropenai.Usage, contextWindow int) adapteropenai.Usage {
	if contextWindow > 0 {
		u.MaxTokens = contextWindow
	}
	return u
}

// buildErrorBodyForUpstream maps a typed Anthropic *UpstreamError into
// the OpenAI ErrorBody shape used by both the SSE native error frame
// and the JSON error envelope.
//
// Type and code are derived from the four-class classification:
//   - retryable + 429 -> rate_limit_error
//   - retryable + 5xx -> server_error
//   - retryable + transport -> server_error (no upstream status)
//   - fatal -> server_error
//
// Message preserves the human-readable upstream text so users still
// see the friendly rate-limit body when one was available.
func buildErrorBodyForUpstream(ue *anthropic.UpstreamError) adapteropenai.ErrorBody {
	if ue == nil {
		return adapteropenai.ErrorBody{Type: "server_error", Code: "upstream_failed", Message: "anthropic upstream error"}
	}
	body := adapteropenai.ErrorBody{Message: ue.Error()}
	switch {
	case ue.Class() == anthropic.ResponseClassRetryableError && ue.Status == 429:
		body.Type = "rate_limit_error"
		body.Code = "rate_limit_exceeded"
	case ue.Class() == anthropic.ResponseClassRetryableError:
		body.Type = "server_error"
		body.Code = "upstream_unavailable"
	default:
		body.Type = "server_error"
		body.Code = "upstream_failed"
	}
	return body
}
