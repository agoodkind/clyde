package codex

import (
	"strings"

	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// Reasoning selects the upstream effort level and summary format.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type reasoningItemPayload struct {
	ID      string                 `json:"id,omitempty"`
	Summary []reasoningSummaryPart `json:"summary,omitempty"`
	Content []reasoningContentPart `json:"content,omitempty"`
}

type reasoningSummaryPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type reasoningContentPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

const reasoningKindSummary = "summary"

func (p *sseEventParser) handleReasoningOutputItem(
	eventName string,
	item transportItem,
) ssePayloadResult {
	if err := p.emitReasoningPresence(item.string("id")); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}

	if eventName != string(codexSSEEventOutputItemDone) || item == nil {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}

	for _, event := range reasoningEventsFromItem(
		item,
		p.reasoningSummaryDeltaSeen,
		p.reasoningTextDeltaSeen,
	) {
		if event.ReasoningKind == reasoningKindSummary {
			p.reasoningSummaryDeltaSeen = true
		} else {
			p.reasoningTextDeltaSeen = true
		}

		if err := p.emitNormalized(event); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}

	cloned := item.toInputItem()
	p.out.OutputItems = append(p.out.OutputItems, cloned)
	// Each item has its own summary and optional encrypted payload.
	p.reasoningSummaryDeltaSeen = false
	p.reasoningTextDeltaSeen = false
	encrypted := cloned.EncryptedContent
	if p.dropEncryptedContent {
		encrypted = ""
	}

	reasoningFinished := adapterrender.ReasoningFinished{
		ReasoningKind:    "",
		EncryptedContent: encrypted,
		Signature:        "",
		ItemID:           cloned.ID,
		ItemType:         string(codexItemTypeReasoning),
	}
	if err := p.emitNormalized(reasoningFinished); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}

	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleReasoningDelta(
	eventName string,
	raw transportStreamEvent,
) ssePayloadResult {
	if raw.Delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}

	p.reasoningSignaled = true
	p.reasoningVisible = true
	kind := string(reasoningContentTypeText)
	var summaryIdx *int
	if strings.Contains(eventName, reasoningKindSummary) {
		kind = reasoningKindSummary
		summaryIdx = raw.SummaryIndex
		p.reasoningSummaryDeltaSeen = true
	} else {
		p.reasoningTextDeltaSeen = true
	}

	err := p.emitNormalized(adapterrender.ReasoningDelta{
		Text:          raw.Delta,
		ReasoningKind: kind,
		SummaryIndex:  summaryIdx,
		Signature:     "",
		RedactedData:  "",
		ItemID:        strings.TrimSpace(raw.ItemID),
		ItemType:      string(codexItemTypeReasoning),
	})
	if err != nil {
		var failed RunResult

		return ssePayloadResult{Action: ssePayloadReturn, Result: failed, Err: err}
	}

	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleReasoningSummaryPartAdded(
	raw transportStreamEvent,
) ssePayloadResult {
	if err := p.emitReasoningPresence(raw.ItemID); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}

	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) emitReasoningPresence(itemID string) error {
	if p.reasoningSignaled {
		return nil
	}

	p.reasoningSignaled = true
	p.reasoningVisible = true

	return p.emitNormalized(adapterrender.ReasoningSignaled{
		ReasoningKind: "",
		ItemID:        strings.TrimSpace(itemID),
		ItemType:      string(codexItemTypeReasoning),
	})
}
