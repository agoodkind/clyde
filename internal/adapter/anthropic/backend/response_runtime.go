package anthropicbackend

import (
	"context"
	"log/slog"
	"slices"
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
	anthUsage, anthStopReason, finishReason, err := RunTranslatorEvents(
		rt.AnthropicStreamClient(),
		ctx,
		req,
		model,
		reqID,
		emit,
	)
	if err != nil {
		return CollectExecutionResult{}, err
	}
	if err := flush(); err != nil {
		return CollectExecutionResult{}, err
	}
	rawUsage := usageWithContextWindow(UsageFromAnthropic(anthUsage), model.Context)
	tracked := rt.TrackAnthropicContextUsage(trackerKey, rawUsage)
	usage := tracked.Usage
	if usage.PromptTokens != rawUsage.PromptTokens || usage.TotalTokens != rawUsage.TotalTokens {
		rt.Log().LogAttrs(ctx, slog.LevelInfo, "adapter.context_usage.tracked",
			slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("alias", model.Alias),
			slog.Int("raw_prompt_tokens", tracked.RawPrompt),
			slog.Int("raw_total_tokens", tracked.RawTotal),
			slog.Int("rolled_output_tokens", tracked.RolledFrom),
			slog.Int("surfaced_prompt_tokens", usage.PromptTokens),
			slog.Int("surfaced_total_tokens", usage.TotalTokens),
		)
	}
	rt.LogCacheUsageAnthropic(ctx, "anthropic", reqID, model.Alias, anthUsage)
	durationMs := time.Since(started).Milliseconds()
	adapterruntime.LogCompleted(rt.Log(), ctx, adapterruntime.CompletedAttrs{
		Backend:             "anthropic",
		Provider:            "anthropic-oauth",
		Path:                "oauth",
		SessionID:           reqID,
		RequestID:           reqID,
		Alias:               model.Alias,
		ModelID:             req.Model,
		FinishReason:        finishReason,
		TokensIn:            usage.PromptTokens,
		TokensOut:           usage.CompletionTokens,
		CacheReadTokens:     anthUsage.CacheReadInputTokens,
		CacheCreationTokens: anthUsage.CacheCreationInputTokens,
		CacheTTL:            rt.CacheTTL(),
		DurationMs:          durationMs,
		Stream:              false,
	})
	breakdown := adapterruntime.EstimateCost(adapterruntime.CostInputs{
		ModelID:             req.Model,
		TTL:                 rt.CacheTTL(),
		InputTokens:         usage.PromptTokens,
		OutputTokens:        usage.CompletionTokens,
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
		TokensIn:            usage.PromptTokens,
		TokensOut:           usage.CompletionTokens,
		CacheReadTokens:     anthUsage.CacheReadInputTokens,
		CacheCreationTokens: anthUsage.CacheCreationInputTokens,
		CostMicrocents:      breakdown.TotalMicrocents,
		DurationMs:          durationMs,
	})
	var events []adapterrender.Event
	if collectedEvents != nil {
		events = collectedEvents()
	}
	return CollectExecutionResult{
		Events:              events,
		Usage:               usage,
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
	anthUsage, _, finishReason, err := RunTranslatorEvents(
		rt.AnthropicStreamClient(),
		ctx,
		req,
		model,
		reqID,
		emitTracked,
	)
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
		if slices.Contains(*dst, value) {
			continue
		}
		*dst = append(*dst, value)
	}
}

func usageWithContextWindow(usage adapteropenai.Usage, contextWindow int) adapteropenai.Usage {
	if contextWindow > 0 {
		usage.MaxTokens = contextWindow
	}
	return usage
}
