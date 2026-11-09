package codex

import (
	"context"
	"encoding/json"
	"strings"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func newSSEEventParser(
	ctx context.Context,
	emit func(adapterrender.Event) error,
	logCtx sseInstrumentationContext,
) *sseEventParser {
	return &sseEventParser{
		logContext:                func() context.Context { return ctx },
		emit:                      emit,
		logCtx:                    logCtx,
		out:                       NewRunResult("stop"),
		toolCallsByItemID:         make(map[string]*toolCallState),
		tools:                     newToolHandlers(newDeclaredToolResolver(nil)),
		dropEncryptedContent:      false,
		nativePatchRepresentation: "",
		reasoningSignaled:         false,
		reasoningVisible:          false,
		reasoningTextDeltaSeen:    false,
		reasoningSummaryDeltaSeen: false,
		nextToolIndex:             0,
		aggregate: sseAggregateCollector{
			Assistant: assistantTextDeltaAggregate{
				DeltaCount:   0,
				CharCount:    0,
				FirstPreview: "",
				LastPreview:  "",
				NormalizedText: strings.
					Builder{},
				PendingWhitespace: false,
			},
			Items: outputItemAggregate{
				Added: outputItemCounts{Total: 0, ToolTotal: 0, TypeCounts: nil},
				Done:  outputItemCounts{Total: 0, ToolTotal: 0, TypeCounts: nil},
			},
			Tools: toolDeltaAggregate{
				FunctionArgumentDeltaCount: 0,
				FunctionArgumentDeltaChars: 0,
				CustomToolInputDeltaCount:  0,
				CustomToolInputDeltaChars:  0,
			},
			Reasoning: reasoningAggregate{
				DeltaCount:        0,
				DeltaChars:        0,
				TextDeltaCount:    0,
				TextDeltaChars:    0,
				SummaryDeltaCount: 0,
				SummaryDeltaChars: 0,
			},
		},

		upstreamEventSeq: 0,
	}
}

func (p *sseEventParser) processPayload(
	ctx context.Context,
	eventName string,
	payload string,
) ssePayloadResult {
	if strings.TrimSpace(payload) == "[DONE]" {
		return ssePayloadResult{Action: ssePayloadBreak, Result: p.out, Err: nil}
	}

	var raw transportStreamEvent
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}

	p.upstreamEventSeq++
	p.aggregate.observe(eventName, raw)

	return p.handleEvent(ctx, eventName, payload, raw)
}
