package codex

import (
	"context"
	"log/slog"

	"goodkind.io/clyde/codexwire"
	"goodkind.io/gklog/correlation"
)

func logCachedWebsocketFrame(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	hit bool,
	log *slog.Logger,
	conv string,
	fullInput []codexwire.InputItem,
) {
	logWebsocketPrepared(
		ctx,
		cfg,
		payload,
		TransportTelemetry{
			WebsocketConnectionReuse: hit,
			RequestID:                "",
			CursorRequestID:          "",
			Correlation: correlation.
				Context{
				TraceID:            "",
				SpanID:             "",
				ParentSpanID:       "",
				RequestID:          "",
				IdentityAttributes: nil,
			},

			Alias:                  "",
			UpstreamModel:          "",
			Transport:              "",
			ServiceTier:            "",
			PromptCacheKey:         "",
			ClientMetadata:         nil,
			InputCount:             0,
			ToolCount:              0,
			NativeShellCount:       0,
			NativeCustomCount:      0,
			FunctionToolCount:      0,
			WebsocketWarmup:        false,
			WebsocketPrewarmUsed:   false,
			WebsocketPrewarmFailed: false,
			PreviousResponseID:     "",
			TurnStatePresent:       false,
			FallbackToHTTP:         false,
			ContextWindowError:     false,
		},
	)
	log.InfoContext(
		ctx,
		"adapter.codex.frame.sent",
		"concern",
		"adapter.providers.codex.request",
		"component",
		"adapter",
		"subcomponent",
		"codex",
		"conversation_id",
		conv,
		"request_id",
		cfg.RequestID,
		"prev_response_id",
		payload.PreviousResponseID,
		"delta_input_count",
		len(payload.Input),
		"full_input_count",
		len(fullInput),
		"is_warmup",
		false,
	)
}
