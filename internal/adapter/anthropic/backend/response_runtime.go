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

type ExecutionRuntime interface {
	Log() *slog.Logger
	AnthropicStreamClient() StreamClient
	TrackAnthropicContextUsage(string, adapteropenai.Usage) TrackedUsage
	LogTerminal(context.Context, adapterruntime.RequestEvent)
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
	usage := trackAndLogAnthropicContextUsage(rt, ctx, anthUsage, model, reqID, trackerKey)
	finalizeAnthropicExecution(rt, ctx, anthropicExecutionFinalize{
		Req:          req,
		Model:        model,
		ReqID:        reqID,
		Started:      started,
		Usage:        usage,
		AnthUsage:    anthUsage,
		FinishReason: finishReason,
		Stream:       false,
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
	started time.Time,
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
	finalUsage := trackAndLogAnthropicContextUsage(rt, ctx, anthUsage, model, reqID, trackerKey)
	finalizeAnthropicExecution(rt, ctx, anthropicExecutionFinalize{
		Req:          req,
		Model:        model,
		ReqID:        reqID,
		Started:      started,
		Usage:        finalUsage,
		AnthUsage:    anthUsage,
		FinishReason: finishReason,
		Stream:       true,
	})
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

// trackAndLogAnthropicContextUsage applies the rolled-output tracker to the
// raw upstream usage and emits adapter.context_usage.tracked when the
// surfaced totals differ from the raw upstream values. Both the collect and
// stream entry points share this helper so context-window accounting stays
// in one place.
func trackAndLogAnthropicContextUsage(
	rt ExecutionRuntime,
	ctx context.Context,
	anthUsage anthropic.Usage,
	model adaptermodel.ResolvedModel,
	reqID string,
	trackerKey string,
) adapteropenai.Usage {
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
	return usage
}

type anthropicExecutionFinalize struct {
	Req          anthropic.Request
	Model        adaptermodel.ResolvedModel
	ReqID        string
	Started      time.Time
	Usage        adapteropenai.Usage
	AnthUsage    anthropic.Usage
	FinishReason string
	Stream       bool
}

// finalizeAnthropicExecution emits the two terminal log events that
// follow every Anthropic backend turn: adapter.request.completed via
// runtime.LogCompleted and the runtime RequestEvent terminal record
// carrying the cost estimate. Cache token fields are now carried on
// adapter.chat.completed directly (Phase A2); the prior dedicated
// adapter.cache.usage emit is retired. Both collect and stream paths
// must call this so the streaming surface (which is what Cursor BYOK
// uses) does not silently skip cache and completion accounting.
func finalizeAnthropicExecution(rt ExecutionRuntime, ctx context.Context, args anthropicExecutionFinalize) {
	durationMs := time.Since(args.Started).Milliseconds()
	adapterruntime.LogCompleted(rt.Log(), ctx, adapterruntime.CompletedAttrs{
		Backend:               "anthropic",
		Provider:              "anthropic-oauth",
		Path:                  "oauth",
		SessionID:             args.ReqID,
		RequestID:             args.ReqID,
		Alias:                 args.Model.Alias,
		ModelID:               args.Req.Model,
		FinishReason:          args.FinishReason,
		TokensIn:              args.Usage.PromptTokens,
		TokensOut:             args.Usage.CompletionTokens,
		CacheReadTokens:       args.AnthUsage.CacheReadInputTokens,
		CacheCreationTokens:   args.AnthUsage.CacheCreationInputTokens,
		CacheCreationReported: true,
		CacheTTL:              rt.CacheTTL(),
		DurationMs:            durationMs,
		Stream:                args.Stream,
	})
	breakdown := adapterruntime.EstimateCost(adapterruntime.CostInputs{
		ModelID:             args.Req.Model,
		TTL:                 rt.CacheTTL(),
		InputTokens:         args.Usage.PromptTokens,
		OutputTokens:        args.Usage.CompletionTokens,
		CacheCreationTokens: args.AnthUsage.CacheCreationInputTokens,
		CacheReadTokens:     args.AnthUsage.CacheReadInputTokens,
	})
	rt.LogTerminal(ctx, adapterruntime.RequestEvent{
		Stage:               adapterruntime.RequestStageCompleted,
		Provider:            "anthropic-oauth",
		Backend:             args.Model.Backend,
		RequestID:           args.ReqID,
		Alias:               args.Model.Alias,
		ModelID:             args.Req.Model,
		Stream:              args.Stream,
		FinishReason:        args.FinishReason,
		TokensIn:            args.Usage.PromptTokens,
		TokensOut:           args.Usage.CompletionTokens,
		CacheReadTokens:     args.AnthUsage.CacheReadInputTokens,
		CacheCreationTokens: args.AnthUsage.CacheCreationInputTokens,
		CostMicrocents:      breakdown.TotalMicrocents,
		DurationMs:          durationMs,
	})
}

func eventHasVisibleContent(ev adapterrender.Event) bool {
	switch e := ev.(type) {
	case adapterrender.TextDelta:
		return strings.TrimSpace(e.Text) != "" || e.Text != ""
	case adapterrender.RefusalDelta:
		return strings.TrimSpace(e.Text) != "" || e.Text != ""
	case adapterrender.ReasoningDelta:
		return strings.TrimSpace(e.Text) != "" || e.Text != ""
	case adapterrender.ToolCallDelta:
		return len(e.ToolCalls) > 0
	default:
		return false
	}
}

func toolCallStats(ev adapterrender.Event) (count int, names []string, hasSubagent bool) {
	td, ok := ev.(adapterrender.ToolCallDelta)
	if !ok {
		return 0, nil, false
	}
	for _, tc := range td.ToolCalls {
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
