package anthropicbackend

import (
	"context"
	"encoding/json"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

type StreamClient interface {
	StreamEvents(context.Context, anthropic.Request, anthropic.EventSink) (anthropic.Usage, string, error)
}

func RunTranslatorEvents(
	client StreamClient,
	ctx context.Context,
	anthReq anthropic.Request,
	model adaptermodel.ResolvedModel,
	reqID string,
	emit func(adapterrender.Event) error,
) (anthropic.Usage, string, string, error) {
	tr := NewStreamTranslator(reqID, model.Alias)
	msgStartPayload, err := json.Marshal(struct {
		Message struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}{})
	if err != nil {
		return anthropic.Usage{}, "", "", err
	}
	msgStartEvents, _, _, _, err := tr.HandleEventEvents("message_start", msgStartPayload)
	if err != nil {
		return anthropic.Usage{}, "", "", err
	}
	for _, ev := range msgStartEvents {
		if err := emit(ev); err != nil {
			return anthropic.Usage{}, "", "", err
		}
	}

	var streamStopReason string
	anthUsage, _, err := client.StreamEvents(ctx, anthReq, func(ev anthropic.StreamEvent) error {
		if stopEv, ok := ev.(anthropic.StreamStop); ok {
			streamStopReason = stopEv.StopReason
			return nil
		}
		evName, payload, ok := StreamEventToTranslatorSSE(ev)
		if !ok {
			return nil
		}
		outEvents, _, _, _, handleErr := tr.HandleEventEvents(evName, payload)
		if handleErr != nil {
			return handleErr
		}
		for _, outEvent := range outEvents {
			if err := emit(outEvent); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return anthUsage, streamStopReason, "", err
	}

	mdPayload, err := json.Marshal(struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}{
		Delta: struct {
			StopReason string `json:"stop_reason"`
		}{StopReason: streamStopReason},
		Usage: struct {
			OutputTokens int `json:"output_tokens"`
		}{OutputTokens: anthUsage.OutputTokens},
	})
	if err != nil {
		return anthUsage, streamStopReason, "", err
	}
	mdEvents, _, _, _, err := tr.HandleEventEvents("message_delta", mdPayload)
	if err != nil {
		return anthUsage, streamStopReason, "", err
	}
	for _, ev := range mdEvents {
		if err := emit(ev); err != nil {
			return anthUsage, streamStopReason, "", err
		}
	}

	stopEvents, _, finishReason, _, err := tr.HandleEventEvents("message_stop", []byte("{}"))
	if err != nil {
		return anthropic.Usage{}, streamStopReason, "", err
	}
	for _, ev := range stopEvents {
		if err := emit(ev); err != nil {
			return anthropic.Usage{}, streamStopReason, "", err
		}
	}
	return anthUsage, streamStopReason, finishReason, nil
}
