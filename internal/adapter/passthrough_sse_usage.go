package adapter

import "bytes"

type passthroughUsageField string

const (
	passthroughUsageInput  passthroughUsageField = "input_tokens"
	passthroughUsageOutput passthroughUsageField = "output_tokens"
	passthroughUsageTotal  passthroughUsageField = "total_tokens"
	passthroughUsageCached passthroughUsageField = "cached_tokens" // #nosec G101 -- JSON usage field, not a credential.
	passthroughUsageNone   passthroughUsageField = ""
	passthroughUsageWindow                       = 64
)

type passthroughSSEUsageParser struct {
	window         []byte
	pendingField   passthroughUsageField
	number         int
	readingNumber  bool
	eventCompleted bool
	previousByte   byte
	eventUsage     Usage
	terminalUsage  Usage
}

func newPassthroughSSEUsageParser() *passthroughSSEUsageParser {
	var eventUsage Usage
	var terminalUsage Usage
	return &passthroughSSEUsageParser{
		window: make([]byte, 0, passthroughUsageWindow), pendingField: passthroughUsageNone,
		number: 0, readingNumber: false, eventCompleted: false, previousByte: 0,
		eventUsage: eventUsage, terminalUsage: terminalUsage,
	}
}

func (p *passthroughSSEUsageParser) Write(chunk []byte) {
	for _, current := range chunk {
		p.consumeByte(current)
	}
}

func (p *passthroughSSEUsageParser) consumeByte(current byte) {
	p.window = append(p.window, current)
	if len(p.window) > passthroughUsageWindow {
		copy(p.window, p.window[len(p.window)-passthroughUsageWindow:])
		p.window = p.window[:passthroughUsageWindow]
	}
	if bytes.HasSuffix(p.window, []byte(`"response.completed"`)) {
		p.eventCompleted = true
	}
	p.consumeNumber(current)
	if p.pendingField == passthroughUsageNone {
		p.detectUsageField()
	}
	if current == '\n' && p.previousByte == '\n' {
		p.finishEvent()
	}
	p.previousByte = current
}

func (p *passthroughSSEUsageParser) detectUsageField() {
	for _, field := range []passthroughUsageField{passthroughUsageInput, passthroughUsageOutput, passthroughUsageTotal, passthroughUsageCached} {
		pattern := []byte(`"` + string(field) + `"`)
		if bytes.HasSuffix(p.window, pattern) {
			p.pendingField = field
			p.number = 0
			p.readingNumber = false
			return
		}
	}
}

func (p *passthroughSSEUsageParser) consumeNumber(current byte) {
	if p.pendingField == passthroughUsageNone {
		return
	}
	if current >= '0' && current <= '9' {
		p.readingNumber = true
		p.number = p.number*10 + int(current-'0')
		return
	}
	if p.readingNumber {
		p.applyNumber()
		p.pendingField = passthroughUsageNone
		p.readingNumber = false
	}
}

func (p *passthroughSSEUsageParser) applyNumber() {
	switch p.pendingField {
	case passthroughUsageInput:
		p.eventUsage.InputTokens = p.number
		p.eventUsage.PromptTokens = p.number
	case passthroughUsageOutput:
		p.eventUsage.OutputTokens = p.number
		p.eventUsage.CompletionTokens = p.number
	case passthroughUsageTotal:
		p.eventUsage.TotalTokens = p.number
	case passthroughUsageCached:
		p.eventUsage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: p.number}
	case passthroughUsageNone:
	}
}

func (p *passthroughSSEUsageParser) finishEvent() {
	if p.eventCompleted {
		p.terminalUsage = p.eventUsage
	}
	p.window = p.window[:0]
	p.pendingField = passthroughUsageNone
	p.number = 0
	p.readingNumber = false
	p.eventCompleted = false
	var eventUsage Usage
	p.eventUsage = eventUsage
}

func (p *passthroughSSEUsageParser) Usage() Usage {
	if p.readingNumber {
		p.applyNumber()
	}
	if p.eventCompleted {
		p.terminalUsage = p.eventUsage
	}
	return p.terminalUsage
}
