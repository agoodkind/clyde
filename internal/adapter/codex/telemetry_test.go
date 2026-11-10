package codex

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLogTransportPreparedIncludesParityFields(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	LogTransportPrepared(context.Background(), log, TransportTelemetry{
		RequestID:                "req-1",
		Alias:                    "clyde-gpt-5.4",
		UpstreamModel:            "gpt-5.4",
		Transport:                "responses_websocket",
		ServiceTier:              "priority",
		PromptCacheKey:           "cursor:conv-123",
		ClientMetadata:           map[string]string{"x-codex-window-id": "cursor:conv-123:0"},
		InputCount:               3,
		ToolCount:                2,
		NativeShellCount:         1,
		NativeCustomCount:        0,
		FunctionToolCount:        1,
		WebsocketWarmup:          true,
		WebsocketPrewarmUsed:     true,
		WebsocketConnectionReuse: true,
		PreviousResponseID:       "resp-123",
		TurnStatePresent:         true,
		FallbackToHTTP:           false,
		ContextWindowError:       true,
	})

	out := buf.String()
	for _, want := range []string{
		`"transport":"responses_websocket"`,
		`"service_tier":"priority"`,
		`"has_previous_response_id":true`,
		`"has_turn_state":true`,
		`"websocket_warmup":true`,
		`"websocket_prewarm_used":true`,
		`"websocket_connection_reused":true`,
		`"context_window_error":true`,
		`"input_count":3`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log output missing %q in %s", want, out)
		}
	}
}

func TestLogUsageTelemetryDistinguishesExplicitZeroCachedTokens(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	LogUsageTelemetry(context.Background(), log, UsageTelemetry{
		UsagePresent:               true,
		InputTokens:                100,
		OutputTokens:               8,
		TotalTokens:                108,
		InputTokensDetailsPresent:  true,
		CachedTokens:               0,
		OutputTokensDetailsPresent: true,
		ReasoningOutputTokens:      0,
	}, UsageLogContext{
		RequestID:          "req-1",
		CursorRequestID:    "cursor-1",
		Alias:              "clyde-codex-5.5-high",
		UpstreamModel:      "gpt-5.5",
		Transport:          "responses_websocket",
		ServiceTier:        "priority",
		PromptCacheKey:     "cursor:conv-123",
		PreviousResponseID: "resp-prev",
		ResponseID:         "resp-1",
		ConversationID:     "cursor:conv-123",
	})

	out := buf.String()
	for _, want := range []string{
		`"usage_present":true`,
		`"input_tokens_details_present":true`,
		`"cached_tokens":0`,
		`"explicit_zero_cached_tokens":true`,
		`"has_prompt_cache_key":true`,
		`"has_previous_response_id":true`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log output missing %q in %s", want, out)
		}
	}
}

func TestLogUsageTelemetryDistinguishesOmittedInputTokenDetails(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	LogUsageTelemetry(context.Background(), log, UsageTelemetry{
		UsagePresent: true,
		InputTokens:  100,
		OutputTokens: 8,
		TotalTokens:  108,
	}, UsageLogContext{
		RequestID:     "req-1",
		Alias:         "clyde-codex-5.5-high",
		UpstreamModel: "gpt-5.5",
		Transport:     "responses_websocket",
		ResponseID:    "resp-1",
	})

	out := buf.String()
	for _, want := range []string{
		`"usage_present":true`,
		`"input_tokens_details_present":false`,
		`"cached_tokens":0`,
		`"explicit_zero_cached_tokens":false`,
		`"has_prompt_cache_key":false`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log output missing %q in %s", want, out)
		}
	}
}
