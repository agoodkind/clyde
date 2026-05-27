package runtime

import (
	"context"
	"log/slog"

	"goodkind.io/gklog/correlation"
)

type RequestStage string

const (
	RequestStageStarted      RequestStage = "started"
	RequestStageStreamOpened RequestStage = "stream_opened"
	RequestStageCompleted    RequestStage = "completed"
	RequestStageFailed       RequestStage = "failed"
	RequestStageCancelled    RequestStage = "cancelled"
)

type RequestEvent struct {
	Stage                      RequestStage
	Provider                   string
	Backend                    string
	RequestID                  string
	Alias                      string
	ModelID                    string
	Stream                     bool
	FinishReason               string
	TokensIn                   int
	TokensOut                  int
	CacheReadTokens            int
	CacheCreationTokens        int
	DerivedCacheCreationTokens int
	ToolCallCount              int
	ToolCallNames              []string
	HasSubagentToolCall        bool
	CostMicrocents             int64
	DurationMs                 int64
	Err                        string
	Correlation                correlation.Context
}

type RequestEventSink func(context.Context, RequestEvent)

type CompletedAttrs struct {
	Backend                    string
	RequestID                  string
	Alias                      string
	ModelID                    string
	FinishReason               string
	TokensIn                   int
	TokensOut                  int
	CacheReadTokens            int
	CacheCreationTokens        int
	DerivedCacheCreationTokens int
	CacheCreationReported      bool
	DurationMs                 int64
	Stream                     bool
	ToolCallCount              int
	ToolCallNames              []string
	HasSubagentToolCall        bool
	Path                       string
	SessionID                  string
	CacheTTL                   string
	Provider                   string
	Correlation                correlation.Context
}

func LogCompleted(log *slog.Logger, ctx context.Context, attrs CompletedAttrs) {
	if log == nil {
		return
	}
	if attrs.Backend == "" {
		attrs.Backend = "unknown"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	breakdown := EstimateCost(CostInputs{
		ModelID:             attrs.ModelID,
		TTL:                 attrs.CacheTTL,
		InputTokens:         attrs.TokensIn,
		OutputTokens:        attrs.TokensOut,
		CacheCreationTokens: attrs.CacheCreationTokens,
		CacheReadTokens:     attrs.CacheReadTokens,
	})
	hitRatio := 0.0
	if denom := attrs.TokensIn + attrs.CacheReadTokens; denom > 0 {
		hitRatio = float64(attrs.CacheReadTokens) / float64(denom)
	}
	corr := attrs.Correlation
	if corr.TraceID == "" {
		corr = correlation.FromContext(ctx)
	}
	args := []slog.Attr{
		slog.String("backend", attrs.Backend),
		slog.String("path", attrs.Path),
		slog.String("session_id", attrs.SessionID),
		slog.String("request_id", attrs.RequestID),
		slog.String("alias", attrs.Alias),
		slog.String("model_id", attrs.ModelID),
		slog.String("finish_reason", attrs.FinishReason),
		slog.Int("prompt_tokens", attrs.TokensIn),
		slog.Int("completion_tokens", attrs.TokensOut),
		slog.Int("cache_read_tokens", attrs.CacheReadTokens),
		slog.Int("cache_creation_tokens", attrs.CacheCreationTokens),
		slog.Int("derived_cache_creation_tokens", attrs.DerivedCacheCreationTokens),
		slog.Bool("cache_creation_reported", attrs.CacheCreationReported),
		slog.String("cache_ttl", attrs.CacheTTL),
		slog.Float64("cache_hit_ratio", hitRatio),
		slog.Int64("duration_ms", attrs.DurationMs),
		slog.Bool("stream", attrs.Stream),
		slog.Int("tool_call_count", attrs.ToolCallCount),
		slog.Any("tool_call_names", attrs.ToolCallNames),
		slog.Bool("has_subagent_tool_call", attrs.HasSubagentToolCall),
		slog.Bool("cost_rates_known", breakdown.RatesKnown),
		slog.Int64("cost_microcents", breakdown.TotalMicrocents),
		slog.Int64("cost_input_microcents", breakdown.InputMicrocents),
		slog.Int64("cost_output_microcents", breakdown.OutputMicrocents),
		slog.Int64("cost_cache_write_microcents", breakdown.CacheWriteMicrocents),
		slog.Int64("cost_cache_read_microcents", breakdown.CacheReadMicrocents),
		slog.Int64("cost_nocache_microcents", breakdown.HypotheticalNoCacheMicrocents),
		slog.Int64("cost_cache_savings_microcents", breakdown.CacheSavingsMicrocents),
	}
	args = append(args, corr.Attrs()...)
	args = append(args, slog.String("model", attrs.ModelID))
	log.LogAttrs(ctx, slog.LevelInfo, "adapter.chat.completed", args...)
}

type StartedAttrs struct {
	Provider    string
	Backend     string
	RequestID   string
	Alias       string
	ModelID     string
	Stream      bool
	Correlation correlation.Context
}

func LogStarted(log *slog.Logger, ctx context.Context, sink RequestEventSink, attrs StartedAttrs) {
	if log == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	corr := attrs.Correlation
	if corr.TraceID == "" {
		corr = correlation.FromContext(ctx)
	}
	logAttrs := []slog.Attr{
		slog.String("provider", attrs.Provider),
		slog.String("backend", attrs.Backend),
		slog.String("request_id", attrs.RequestID),
		slog.String("alias", attrs.Alias),
		slog.String("model", attrs.ModelID),
		slog.Bool("stream", attrs.Stream),
	}
	logAttrs = append(logAttrs, corr.Attrs()...)
	log.LogAttrs(ctx, slog.LevelInfo, "adapter.request.started", logAttrs...)
	if sink != nil {
		sink(ctx, RequestEvent{
			Stage:       RequestStageStarted,
			Provider:    attrs.Provider,
			Backend:     attrs.Backend,
			RequestID:   attrs.RequestID,
			Alias:       attrs.Alias,
			ModelID:     attrs.ModelID,
			Stream:      attrs.Stream,
			Correlation: corr,
		})
	}
}

type StreamOpenedAttrs struct {
	Provider    string
	Backend     string
	RequestID   string
	Alias       string
	ModelID     string
	Stream      bool
	Correlation correlation.Context
}

func LogStreamOpened(log *slog.Logger, ctx context.Context, sink RequestEventSink, attrs StreamOpenedAttrs) {
	if log == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	corr := attrs.Correlation
	if corr.TraceID == "" {
		corr = correlation.FromContext(ctx)
	}
	logAttrs := []slog.Attr{
		slog.String("provider", attrs.Provider),
		slog.String("backend", attrs.Backend),
		slog.String("request_id", attrs.RequestID),
		slog.String("alias", attrs.Alias),
		slog.String("model", attrs.ModelID),
		slog.Bool("stream", attrs.Stream),
	}
	logAttrs = append(logAttrs, corr.Attrs()...)
	log.LogAttrs(ctx, slog.LevelInfo, "adapter.request.stream_opened", logAttrs...)
	if sink != nil {
		sink(ctx, RequestEvent{
			Stage:       RequestStageStreamOpened,
			Provider:    attrs.Provider,
			Backend:     attrs.Backend,
			RequestID:   attrs.RequestID,
			Alias:       attrs.Alias,
			ModelID:     attrs.ModelID,
			Stream:      attrs.Stream,
			Correlation: corr,
		})
	}
}

func LogTerminal(log *slog.Logger, ctx context.Context, sink RequestEventSink, ev RequestEvent) {
	if log == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	msg := "adapter.request.completed"
	level := slog.LevelInfo
	switch ev.Stage {
	case RequestStageFailed:
		msg = "adapter.request.failed"
		level = slog.LevelWarn
	case RequestStageCancelled:
		msg = "adapter.request.cancelled"
	}
	corr := ev.Correlation
	if corr.TraceID == "" {
		corr = correlation.FromContext(ctx)
	}
	logAttrs := []slog.Attr{
		slog.String("provider", ev.Provider),
		slog.String("backend", ev.Backend),
		slog.String("request_id", ev.RequestID),
		slog.String("alias", ev.Alias),
		slog.String("model", ev.ModelID),
		slog.Bool("stream", ev.Stream),
		slog.String("finish_reason", ev.FinishReason),
		slog.Int("prompt_tokens", ev.TokensIn),
		slog.Int("completion_tokens", ev.TokensOut),
		slog.Int("cache_read_tokens", ev.CacheReadTokens),
		slog.Int("cache_creation_tokens", ev.CacheCreationTokens),
		slog.Int("derived_cache_creation_tokens", ev.DerivedCacheCreationTokens),
		slog.Int("tool_call_count", ev.ToolCallCount),
		slog.Any("tool_call_names", ev.ToolCallNames),
		slog.Bool("has_subagent_tool_call", ev.HasSubagentToolCall),
		slog.Int64("cost_microcents", ev.CostMicrocents),
		slog.Int64("duration_ms", ev.DurationMs),
		slog.String("error", ev.Err),
	}
	logAttrs = append(logAttrs, corr.Attrs()...)
	log.LogAttrs(ctx, level, msg, logAttrs...)
	if sink != nil {
		ev.Correlation = corr
		sink(ctx, ev)
	}
}
