package adapter

import (
	"encoding/json"
	"fmt"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
)

func (p *responsesStreamWriter) nextSeq() int {
	seq := p.seq
	p.seq++

	return seq
}

func (p *responsesStreamWriter) send(name string, payload []byte) error {
	if err := p.sse.WriteNamedEvent(name, payload); err != nil {
		if p.log != nil {
			p.log.Warn(
				"adapter.responses.emit_failed",
				"concern",
				"adapter.chat.render",
				"event",
				name,
				"err",
				err,
			)
		}

		return fmt.Errorf("write responses named event %q: %w", name, err)
	}

	return nil
}

// begin emits response.created and response.in_progress with the
// in-progress response object. It is idempotent so the dispatch path
// and WriteEvent can both call it safely.
func (p *responsesStreamWriter) begin() error {
	if p.began {
		return nil
	}

	p.began = true
	inProgress := p.buildResponse(adapteropenai.ResponsesStatusInProgress, nil)
	// Warnings appear only on response.created. Every later event avoids
	// repeating the same compatibility metadata.
	if len(p.warnings) > 0 {
		inProgress.Clyde = &adapteropenai.ResponsesClyde{Warnings: p.warnings}
	}

	if err := p.emitEnvelope(adapteropenai.ResponsesEventCreated, inProgress); err != nil {
		return err
	}

	inProgress.Clyde = nil

	return p.emitEnvelope(adapteropenai.ResponsesEventInProgress, inProgress)
}

// finish closes any open reasoning, message, and function-call items, then
// emits the terminal response.completed or response.incomplete event.
func (p *responsesStreamWriter) finish(result adapterprovider.Result) error {
	if err := p.begin(); err != nil {
		return err
	}

	if p.documented {
		if missing := adapteropenai.MissingResponsesUsageDetail(result.Usage); missing != "" {
			return p.fail(
				adapterErrUpstreamFailed("", "model provider omitted usage."+missing, nil),
			)
		}
	}

	status, incompleteDetails := adapteropenai.ResponsesTerminalForFinishReason(result.FinishReason)
	itemStatus := responsesTerminalItemStatus(status)
	if err := p.closeReasoning(itemStatus); err != nil {
		return err
	}

	if err := p.closeMessage(itemStatus); err != nil {
		return err
	}

	if err := p.closeTools(itemStatus); err != nil {
		return err
	}

	usage := result.Usage
	final := p.buildResponse(status, &usage)
	final.IncompleteDetails = incompleteDetails
	eventName := adapteropenai.ResponsesEventCompleted
	if status == adapteropenai.ResponsesStatusIncomplete {
		eventName = adapteropenai.ResponsesEventIncomplete
	}

	return p.emitEnvelope(eventName, final)
}

func responsesTerminalItemStatus(
	status adapteropenai.ResponsesStatus,
) adapteropenai.ResponsesOutputItemStatus {
	if status == adapteropenai.ResponsesStatusIncomplete {
		return adapteropenai.ResponsesOutputItemStatusIncomplete
	}

	return adapteropenai.ResponsesOutputItemStatusCompleted
}

func (p *responsesStreamWriter) emitEnvelope(
	name string,
	resp adapteropenai.ResponsesResponse,
) error {
	evt := adapteropenai.ResponsesEnvelopeEvent{
		Type:           name,
		Response:       resp,
		SequenceNumber: p.nextSeq(),
	}

	return p.marshalSend(name, evt)
}

func (p *responsesStreamWriter) emitOutputItem(
	name string,
	outputIndex int,
	item adapteropenai.ResponsesOutputItem,
) error {
	evt := adapteropenai.ResponsesOutputItemEvent{
		Type:           name,
		OutputIndex:    outputIndex,
		Item:           item,
		SequenceNumber: p.nextSeq(),
	}

	return p.marshalSend(name, evt)
}

// marshalSend serializes one typed Responses event and writes it as a
// named SSE frame.
func (p *responsesStreamWriter) marshalSend(
	name string,
	evt adapteropenai.ResponsesStreamEvent,
) error {
	payload, err := json.Marshal(evt)
	if err != nil {
		if p.log != nil {
			p.log.Warn(
				"adapter.responses.marshal_failed",
				"concern",
				"adapter.chat.render",
				"event",
				name,
				"err",
				err,
			)
		}

		return fmt.Errorf("marshal responses event %q: %w", name, err)
	}

	return p.send(name, payload)
}
