package adapter

import "bytes"

func (p *passthroughSSEUsageParser) resolveStreamPrefix() {
	if p.streamPrefixResolved {
		return
	}

	p.streamPrefixResolved = true
	if p.streamPrefixLength == len(p.streamPrefix) && p.streamPrefix == [3]byte{0xef, 0xbb, 0xbf} {
		return
	}

	for i := range p.streamPrefixLength {
		p.consumeByte(p.streamPrefix[i])
	}
}

func (p *passthroughSSEUsageParser) consumeLineNameByte(current byte) {
	if current == ':' {
		p.lineHeaderComplete = true
		p.lineKind = p.currentLineKind()

		return
	}

	if p.lineNameLength < len(p.lineName) {
		p.lineName[p.lineNameLength] = current
	} else {
		p.lineNameTooLong = true
	}

	p.lineNameLength++
}

func (p *passthroughSSEUsageParser) currentLineKind() passthroughSSELineKind {
	if p.lineNameTooLong {
		return passthroughSSELineUnknown
	}

	name := p.lineName[:p.lineNameLength]
	if bytes.Equal(name, []byte("data")) {
		return passthroughSSELineData
	}

	if bytes.Equal(name, []byte("event")) {
		return passthroughSSELineEvent
	}

	return passthroughSSELineUnknown
}

func (p *passthroughSSEUsageParser) consumeLineValueByte(current byte) {
	if !p.lineValueStarted {
		p.lineValueStarted = true
		if current == ' ' {
			return
		}
	}

	switch p.lineKind {
	case passthroughSSELineData:
		p.json.consumeByte(current)
	case passthroughSSELineEvent:
		p.consumeEventNameByte(current)
	case passthroughSSELineUnknown:
	}
}

func (p *passthroughSSEUsageParser) consumeEventNameByte(current byte) {
	if p.eventNameLength < len(p.eventName) {
		p.eventName[p.eventNameLength] = current
	} else {
		p.eventNameTooLong = true
	}

	p.eventNameLength++
}

func (p *passthroughSSEUsageParser) finishLine() {
	if !p.lineHeaderComplete && p.lineNameLength == 0 {
		p.finishEvent()
		p.resetLine()

		return
	}

	if !p.lineHeaderComplete {
		p.lineKind = p.currentLineKind()
	}

	switch p.lineKind {
	case passthroughSSELineData:
		p.json.consumeByte('\n')
	case passthroughSSELineEvent:
		if p.eventNameTooLong {
			p.eventTerminal = passthroughTerminalEventNone
		} else {
			p.eventTerminal = passthroughTerminalEventForName(p.eventName[:p.eventNameLength])
		}
	case passthroughSSELineUnknown:
	}

	p.resetLine()
}

func (p *passthroughSSEUsageParser) finishEvent() {
	p.json.Finish()
	if p.eventTerminal != passthroughTerminalEventNone && p.json.terminal == p.eventTerminal &&
		p.json.Valid() {
		p.terminalUsage = p.json.usage
	}

	p.json.Reset()
	p.eventTerminal = passthroughTerminalEventNone
}

func (p *passthroughSSEUsageParser) resetLine() {
	p.lineKind = passthroughSSELineUnknown
	p.lineNameLength = 0
	p.lineNameTooLong = false
	p.lineHeaderComplete = false
	p.lineValueStarted = false
	p.eventNameLength = 0
	p.eventNameTooLong = false
}
