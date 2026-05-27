package codex

import (
	"context"
	"log/slog"

	"goodkind.io/gklog/correlation"
)

type TransportTelemetry struct {
	RequestID                string
	CursorRequestID          string
	Correlation              correlation.Context
	Alias                    string
	UpstreamModel            string
	Transport                string
	ServiceTier              string
	MaxCompletion            *int
	PromptCacheKey           string
	ClientMetadata           map[string]string
	InputCount               int
	ToolCount                int
	NativeShellCount         int
	NativeCustomCount        int
	FunctionToolCount        int
	WebsocketWarmup          bool
	WebsocketPrewarmUsed     bool
	WebsocketPrewarmFailed   bool
	WebsocketConnectionReuse bool
	PreviousResponseID       string
	TurnStatePresent         bool
	FallbackToHTTP           bool
	ContextWindowError       bool
}

type CodexUsageLogContext struct {
	RequestID          string
	CursorRequestID    string
	Correlation        correlation.Context
	Alias              string
	UpstreamModel      string
	Transport          string
	ServiceTier        string
	PromptCacheKey     string
	PreviousResponseID string
	ResponseID         string
	ConversationID     string
	WebsocketWarmup    bool
}

func LogTransportPrepared(ctx context.Context, log *slog.Logger, telemetry TransportTelemetry) {
	if log == nil {
		log = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("component", "adapter"),
		slog.String("subcomponent", "codex"),
		slog.String("request_id", telemetry.RequestID),
		slog.String("cursor_request_id", telemetry.CursorRequestID),
		slog.String("alias", telemetry.Alias),
		slog.String("model", telemetry.UpstreamModel),
		slog.String("transport", telemetry.Transport),
		slog.String("service_tier", telemetry.ServiceTier),
		slog.Any("max_completion_tokens", telemetry.MaxCompletion),
		slog.Bool("has_prompt_cache_key", telemetry.PromptCacheKey != ""),
		slog.Bool("has_client_metadata", len(telemetry.ClientMetadata) > 0),
		slog.Int("input_count", telemetry.InputCount),
		slog.Int("tool_count", telemetry.ToolCount),
		slog.Int("native_local_shell_count", telemetry.NativeShellCount),
		slog.Int("native_custom_count", telemetry.NativeCustomCount),
		slog.Int("function_tool_count", telemetry.FunctionToolCount),
		slog.Bool("websocket_warmup", telemetry.WebsocketWarmup),
		slog.Bool("websocket_prewarm_used", telemetry.WebsocketPrewarmUsed),
		slog.Bool("websocket_prewarm_failed", telemetry.WebsocketPrewarmFailed),
		slog.Bool("websocket_connection_reused", telemetry.WebsocketConnectionReuse),
		slog.Bool("has_previous_response_id", telemetry.PreviousResponseID != ""),
		slog.Bool("has_turn_state", telemetry.TurnStatePresent),
		slog.Bool("fallback_to_http", telemetry.FallbackToHTTP),
		slog.Bool("context_window_error", telemetry.ContextWindowError),
	}
	attrs = append(attrs, telemetry.Correlation.Attrs()...)
	log.LogAttrs(ctx, slog.LevelInfo, "adapter.codex.transport.prepared", attrs...)
}

func LogUsageTelemetry(ctx context.Context, log *slog.Logger, usage CodexUsageTelemetry, meta CodexUsageLogContext) {
	if log == nil {
		log = slog.Default()
	}
	attrs := []slog.Attr{
		slog.String("component", "adapter"),
		slog.String("subcomponent", "codex"),
		slog.String("request_id", meta.RequestID),
		slog.String("cursor_request_id", meta.CursorRequestID),
		slog.String("alias", meta.Alias),
		slog.String("model", meta.UpstreamModel),
		slog.String("transport", meta.Transport),
		slog.String("service_tier", meta.ServiceTier),
		slog.String("response_id", meta.ResponseID),
		slog.String("conversation_id", meta.ConversationID),
		slog.Bool("websocket_warmup", meta.WebsocketWarmup),
		slog.Bool("has_prompt_cache_key", meta.PromptCacheKey != ""),
		slog.Int("prompt_cache_key_bytes", len(meta.PromptCacheKey)),
		slog.Bool("has_previous_response_id", meta.PreviousResponseID != ""),
		slog.Bool("usage_present", usage.UsagePresent),
		slog.Int("input_tokens", usage.InputTokens),
		slog.Int("output_tokens", usage.OutputTokens),
		slog.Int("total_tokens", usage.TotalTokens),
		slog.Bool("input_tokens_details_present", usage.InputTokensDetailsPresent),
		slog.Int("cached_tokens", usage.CachedTokens),
		slog.Bool("explicit_zero_cached_tokens", usage.InputTokensDetailsPresent && usage.CachedTokens == 0),
		slog.Bool("output_tokens_details_present", usage.OutputTokensDetailsPresent),
		slog.Int("reasoning_output_tokens", usage.ReasoningOutputTokens),
	}
	attrs = append(attrs, meta.Correlation.Attrs()...)
	log.LogAttrs(ctx, slog.LevelInfo, "adapter.codex.usage.completed", attrs...)
}
