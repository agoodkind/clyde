package anthropicbackend

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/adapter/finishreason"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/gklog/correlation"
)

// TrackedUsage is part of Clyde's typed adapter surface.
type TrackedUsage struct {
	Usage      adapteropenai.Usage
	RawPrompt  int
	RawTotal   int
	RolledFrom int
}

// ExecutionRuntime is part of Clyde's typed adapter surface.
type ExecutionRuntime interface {
	Log() *slog.Logger
	AnthropicStreamClient() StreamClient
	TrackAnthropicContextUsage(string, adapteropenai.Usage) TrackedUsage
	LogTerminal(context.Context, adapterruntime.RequestEvent)
	CacheTTL() string
}

// CollectExecutionResult is part of Clyde's typed adapter surface.
type CollectExecutionResult struct {
	Events              []adapterrender.Event
	Usage               adapteropenai.Usage
	FinishReason        string
	AnthropicStopReason string
	AnthropicUsage      anthropic.Usage
}

// StreamExecutionResult is part of Clyde's typed adapter surface.
type StreamExecutionResult struct {
	Usage               adapteropenai.Usage
	FinishReason        string
	AnthropicUsage      anthropic.Usage
	EmittedContent      bool
	ToolCallCount       int
	ToolCallNames       []string
	HasSubagentToolCall bool
}

// RunCollectExecution is part of Clyde's typed adapter surface.
func RunCollectExecution(
	rt ExecutionRuntime,
	ctx context.Context,
	req anthropic.Request,
	resolved *adapterresolver.ResolvedRequest,
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
		resolved,
		reqID,
		emit,
	)
	if err != nil {
		return CollectExecutionResult{}, err
	}
	if err := flush(); err != nil {
		return CollectExecutionResult{}, err
	}
	usage := trackAndLogAnthropicContextUsage(rt, ctx, anthUsage, resolved, reqID, trackerKey)
	finalizeAnthropicExecution(rt, ctx, anthropicExecutionFinalize{
		Req:          req,
		Resolved:     resolved,
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

// RunStreamExecution is part of Clyde's typed adapter surface.
func RunStreamExecution(
	rt ExecutionRuntime,
	ctx context.Context,
	req anthropic.Request,
	resolved *adapterresolver.ResolvedRequest,
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
	anthUsage, anthStopReason, finishReason, err := RunTranslatorEvents(
		rt.AnthropicStreamClient(),
		ctx,
		req,
		resolved,
		reqID,
		emitTracked,
	)
	// Late or interrupted streams may surface as a non-nil err after some
	// content has already streamed. We still want the finalize sequence
	// to receive the accumulated usage and a best-effort finish_reason.
	// Mirror the official Claude Code SSE consumer fallback behavior:
	// if message_stop never arrived but stop_reason was recorded via
	// message_delta or StreamStop, derive the OpenAI finish_reason from
	// that. See research/claude-code-source-code-full/src/services/api/claude.ts
	// around lines 2341-2350 and 2818-2869.
	if finishReason == "" {
		finishReason = finishreason.FromAnthropicStream(anthStopReason)
	}
	finalUsage := trackAndLogAnthropicContextUsage(rt, ctx, anthUsage, resolved, reqID, trackerKey)
	finalizeAnthropicExecution(rt, ctx, anthropicExecutionFinalize{
		Req:          req,
		Resolved:     resolved,
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
	resolved *adapterresolver.ResolvedRequest,
	reqID string,
	trackerKey string,
) adapteropenai.Usage {
	contextWindow := 0
	if resolved != nil {
		contextWindow = resolved.ContextBudget.InputTokens
	}
	rawUsage := usageWithContextWindow(UsageFromAnthropic(anthUsage), contextWindow)
	tracked := rt.TrackAnthropicContextUsage(trackerKey, rawUsage)
	usage := tracked.Usage
	if usage.PromptTokens != rawUsage.PromptTokens || usage.TotalTokens != rawUsage.TotalTokens {
		rt.Log().LogAttrs(ctx, slog.LevelInfo, "adapter.context_usage.tracked", slog.String("concern", "adapter.chat.render"), slog.String("backend", "anthropic"),
			slog.String("request_id", reqID),
			slog.String("alias", requestAlias(resolved)),
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
	Resolved     *adapterresolver.ResolvedRequest
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
// carrying the recorded token and cache counts. Dollar cost is no
// longer precomputed here; the daemon provider-stats aggregator prices
// the recorded tokens at read time from the config pricing table. Cache
// token fields are carried on adapter.chat.completed directly (Phase
// A2); the prior dedicated adapter.cache.usage emit is retired. Both
// collect and stream paths must call this so the streaming surface
// (which is what Cursor BYOK uses) does not silently skip cache and
// completion accounting.
func finalizeAnthropicExecution(rt ExecutionRuntime, ctx context.Context, args anthropicExecutionFinalize) {
	durationMs := clock.Since(args.Started).Milliseconds()
	adapterruntime.LogCompleted(rt.Log(), ctx, adapterruntime.CompletedAttrs{
		Backend:               "anthropic",
		Provider:              "anthropic-oauth",
		Path:                  "oauth",
		SessionID:             args.ReqID,
		RequestID:             args.ReqID,
		Alias:                 requestAlias(args.Resolved),
		ModelID:               args.Req.Model,
		FinishReason:          args.FinishReason,
		TokensIn:              args.Usage.PromptTokens,
		TokensOut:             args.Usage.CompletionTokens,
		CacheReadTokens:       args.AnthUsage.CacheReadInputTokens,
		CacheCreationTokens:   args.AnthUsage.CacheCreationInputTokens,
		CacheCreationReported: true,
		CacheTTL:              rt.CacheTTL(),
		DurationMs:            durationMs,
		Stream:                args.Stream, DerivedCacheCreationTokens: 0, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false, Correlation: correlation.
					Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	})
	rt.LogTerminal(ctx, adapterruntime.RequestEvent{
		Stage:               adapterruntime.RequestStageCompleted,
		Provider:            "anthropic-oauth",
		Backend:             anthropicBackendName(args.Resolved),
		RequestID:           args.ReqID,
		Alias:               requestAlias(args.Resolved),
		ModelID:             args.Req.Model,
		Stream:              args.Stream,
		FinishReason:        args.FinishReason,
		TokensIn:            args.Usage.PromptTokens,
		TokensOut:           args.Usage.CompletionTokens,
		CacheReadTokens:     args.AnthUsage.CacheReadInputTokens,
		CacheCreationTokens: args.AnthUsage.CacheCreationInputTokens,
		DurationMs:          durationMs, DerivedCacheCreationTokens: 0, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false, Err: "", Correlation: correlation.
					Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
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
		switch subagentToolNameAnth(name) {
		case subagentToolNameAnthSubagent, subagentToolNameAnthTask, subagentToolNameAnthSpawnAgent:
			hasSubagent = true
		}
	}
	return count, names, hasSubagent
}

// subagentToolNameAnth enumerates the tool aliases Cursor uses for
// subagent or task-spawning calls when the adapter routes the
// translated response back through the Anthropic backend.
type subagentToolNameAnth string

const (
	subagentToolNameAnthSubagent   subagentToolNameAnth = "Subagent"
	subagentToolNameAnthTask       subagentToolNameAnth = "Task"
	subagentToolNameAnthSpawnAgent subagentToolNameAnth = "spawn_agent"
)

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
