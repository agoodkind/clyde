package anthropicbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// StreamClient is part of Clyde's typed adapter surface.
type StreamClient interface {
	StreamEvents(
		context.Context,
		anthropic.Request,
		anthropic.EventSink,
	) (anthropic.Usage, string, error)
}

// RunTranslatorEvents is part of Clyde's typed adapter surface.
func RunTranslatorEvents(
	client StreamClient,
	ctx context.Context,
	anthReq anthropic.Request,
	resolved *adapterresolver.ResolvedRequest,
	reqID string,
	emit func(adapterrender.Event) error,
) (anthropic.Usage, string, string, error) {
	tr := NewStreamTranslator(ctx, reqID, requestAlias(resolved))
	msgStartPayload, err := json.Marshal(
		struct {
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}{
			Message: struct {
				Usage struct {
					InputTokens  int "json:\"input_tokens\""
					OutputTokens int "json:\"output_tokens\""
				} "json:\"usage\""
			}{
				Usage: struct {
					InputTokens  int "json:\"input_tokens\""
					OutputTokens int "json:\"output_tokens\""
				}{
					InputTokens:  0,
					OutputTokens: 0,
				},
			},
		},
	)
	if err != nil {
		slog.WarnContext(
			ctx,
			"adapter.anthropic.translator.marshal_message_start_failed",
			"concern",
			"adapter.providers.anthropic.request",
			"err",
			err,
		)

		return anthropic.Usage{}, "", "", fmt.Errorf("marshal anthropic message_start: %w", err)
	}

	if _, err := emitTranslatorEvents(tr, "message_start", msgStartPayload, emit); err != nil {
		return anthropic.Usage{}, "", "", err
	}

	var streamStopReason string
	anthUsage, _, err := client.StreamEvents(
		ctx,
		anthReq,
		func(event anthropic.StreamEvent) error {
			if stopEv, ok := event.(anthropic.StreamStop); ok {
				streamStopReason = stopEv.StopReason
				return nil
			}

			evName, payload, ok := StreamEventToTranslatorSSE(event)
			if !ok {
				return nil
			}

			_, err := emitTranslatorEvents(tr, evName, payload, emit)

			return err
		},
	)
	if err != nil {
		slog.WarnContext(
			ctx,
			"adapter.anthropic.translator.stream_events_failed",
			"concern",
			"adapter.providers.anthropic.request",
			"err",
			err,
		)

		return anthUsage, streamStopReason, "", fmt.Errorf(
			"stream anthropic translator events: %w",
			err,
		)
	}

	return finishTranslatorEvents(ctx, tr, anthUsage, &streamStopReason, emit)
}

func finishTranslatorEvents(
	ctx context.Context,
	translator *StreamTranslator,
	anthUsage anthropic.Usage,
	stopReason *string,
	emit func(adapterrender.Event) error,
) (anthropic.Usage, string, string, error) {
	mdPayload, err := json.Marshal(
		struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}{
			Delta: struct {
				StopReason string `json:"stop_reason"`
			}{
				StopReason: *stopReason,
			},
			Usage: struct {
				OutputTokens int `json:"output_tokens"`
			}{
				OutputTokens: anthUsage.OutputTokens,
			},
		},
	)
	if err != nil {
		slog.WarnContext(
			ctx,
			"adapter.anthropic.translator.marshal_message_delta_failed",
			"concern",
			"adapter.providers.anthropic.request",
			"err",
			err,
		)

		return anthUsage, *stopReason, "", fmt.Errorf("marshal anthropic message_delta: %w", err)
	}

	if _, err := emitTranslatorEvents(translator, "message_delta", mdPayload, emit); err != nil {
		return anthUsage, *stopReason, "", err
	}

	finishReason, err := emitTranslatorEvents(translator, "message_stop", []byte("{}"), emit)
	if err != nil {
		return anthropic.Usage{}, *stopReason, "", err
	}

	return anthUsage, *stopReason, finishReason, nil
}

func emitTranslatorEvents(
	tr *StreamTranslator,
	eventName string,
	payload []byte,
	emit func(adapterrender.Event) error,
) (string, error) {
	outEvents, _, finishReason, _, err := tr.HandleEventEvents(eventName, payload)
	if err != nil {
		slog.Warn(
			"adapter.anthropic.translator.handle_event_failed",
			"concern",
			"adapter.providers.anthropic.request",
			"event",
			eventName,
			"err",
			err,
		)

		return "", fmt.Errorf("handle anthropic translator event %s: %w", eventName, err)
	}

	for _, event := range outEvents {
		if err := emit(event); err != nil {
			slog.Warn(
				"adapter.anthropic.translator.emit_event_failed",
				"concern",
				"adapter.providers.anthropic.request",
				"event",
				eventName,
				"err",
				err,
			)

			return "", fmt.Errorf("emit anthropic translator event %s: %w", eventName, err)
		}
	}

	return finishReason, nil
}
