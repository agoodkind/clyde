package adapter

import "bytes"

const (
	passthroughTerminalResponseEvent = "response.completed"
	passthroughMaxJSONDepth          = 64
	passthroughMaxJSONStringBytes    = 64
)

type passthroughSSELineKind uint8

const (
	passthroughSSELineUnknown passthroughSSELineKind = iota
	passthroughSSELineData
	passthroughSSELineEvent
)

type passthroughSSEUsageParser struct {
	json                  passthroughUsageJSONParser
	terminalUsage         Usage
	lineKind              passthroughSSELineKind
	lineName              [5]byte
	lineNameLength        int
	lineNameTooLong       bool
	lineHeaderComplete    bool
	lineValueStarted      bool
	eventNameMatches      bool
	eventNameLength       int
	eventTrailingSpace    bool
	eventTerminal         bool
	skipFollowingLineFeed bool
}

func newPassthroughSSEUsageParser() *passthroughSSEUsageParser {
	var parser passthroughSSEUsageParser
	parser.resetLine()
	return &parser
}

func (p *passthroughSSEUsageParser) Write(chunk []byte) {
	for _, current := range chunk {
		p.consumeByte(current)
	}
}

func (p *passthroughSSEUsageParser) consumeByte(current byte) {
	if p.skipFollowingLineFeed {
		p.skipFollowingLineFeed = false
		if current == '\n' {
			return
		}
	}
	if current == '\r' {
		p.finishLine()
		p.skipFollowingLineFeed = true
		return
	}
	if current == '\n' {
		p.finishLine()
		return
	}
	if !p.lineHeaderComplete {
		p.consumeLineNameByte(current)
		return
	}
	p.consumeLineValueByte(current)
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
		p.json.WriteByte(current)
	case passthroughSSELineEvent:
		p.consumeEventNameByte(current)
	case passthroughSSELineUnknown:
	}
}

func (p *passthroughSSEUsageParser) consumeEventNameByte(current byte) {
	if p.eventTrailingSpace {
		if current != ' ' && current != '\t' {
			p.eventNameMatches = false
		}
		return
	}
	if p.eventNameLength >= len(passthroughTerminalResponseEvent) {
		if current == ' ' || current == '\t' {
			p.eventTrailingSpace = true
			return
		}
		p.eventNameMatches = false
		return
	}
	if current != passthroughTerminalResponseEvent[p.eventNameLength] {
		p.eventNameMatches = false
	}
	p.eventNameLength++
}

func (p *passthroughSSEUsageParser) finishLine() {
	if !p.lineHeaderComplete && p.lineNameLength == 0 {
		p.finishEvent()
		p.resetLine()
		return
	}
	switch p.lineKind {
	case passthroughSSELineData:
		p.json.WriteByte('\n')
	case passthroughSSELineEvent:
		if p.eventNameMatches && p.eventNameLength == len(passthroughTerminalResponseEvent) {
			p.eventTerminal = true
		}
	case passthroughSSELineUnknown:
	}
	p.resetLine()
}

func (p *passthroughSSEUsageParser) finishEvent() {
	p.json.Finish()
	if p.eventTerminal || p.json.terminal {
		p.terminalUsage = p.json.usage
	}
	p.json.Reset()
	p.eventTerminal = false
}

func (p *passthroughSSEUsageParser) resetLine() {
	p.lineKind = passthroughSSELineUnknown
	p.lineNameLength = 0
	p.lineNameTooLong = false
	p.lineHeaderComplete = false
	p.lineValueStarted = false
	p.eventNameMatches = true
	p.eventNameLength = 0
	p.eventTrailingSpace = false
}

func (p *passthroughSSEUsageParser) Usage() Usage {
	if p.lineHeaderComplete || p.lineNameLength > 0 {
		p.finishLine()
	}
	if p.eventTerminal || p.json.hasContent() {
		p.finishEvent()
	}
	return p.terminalUsage
}

type passthroughJSONMode uint8

const (
	passthroughJSONNormal passthroughJSONMode = iota
	passthroughJSONString
	passthroughJSONNumber
	passthroughJSONLiteral
)

type passthroughJSONContainer uint8

const (
	passthroughJSONObject passthroughJSONContainer = iota
	passthroughJSONArray
)

type passthroughUsageKey uint8

const (
	passthroughUsageKeyNone passthroughUsageKey = iota
	passthroughUsageKeyResponse
	passthroughUsageKeyUsage
	passthroughUsageKeyInputTokens
	passthroughUsageKeyOutputTokens
	passthroughUsageKeyTotalTokens
	passthroughUsageKeyInputTokensDetails
	passthroughUsageKeyCachedTokens
	passthroughUsageKeyType
)

type passthroughJSONFrame struct {
	container    passthroughJSONContainer
	pathKey      passthroughUsageKey
	currentKey   passthroughUsageKey
	expectingKey bool
}

type passthroughUsageJSONParser struct {
	frames        [passthroughMaxJSONDepth]passthroughJSONFrame
	depth         int
	overflowDepth int
	mode          passthroughJSONMode
	stringBuffer  [passthroughMaxJSONStringBytes]byte
	stringLength  int
	stringTooLong bool
	stringEscaped bool
	stringEscape  bool
	number        int
	numberDigits  bool
	numberValid   bool
	terminal      bool
	usage         Usage
	contentSeen   bool
}

func (p *passthroughUsageJSONParser) WriteByte(current byte) {
	p.contentSeen = true
	switch p.mode {
	case passthroughJSONNormal:
		p.consumeNormalByte(current)
	case passthroughJSONString:
		p.consumeStringByte(current)
	case passthroughJSONNumber:
		p.consumeNumberByte(current)
	case passthroughJSONLiteral:
		p.consumeLiteralByte(current)
	}
}

func (p *passthroughUsageJSONParser) consumeNormalByte(current byte) {
	switch current {
	case ' ', '\t', '\n', '\r', ':':
		return
	case '{':
		p.enterContainer(passthroughJSONObject)
	case '[':
		p.enterContainer(passthroughJSONArray)
	case '}', ']':
		p.leaveContainer()
	case ',':
		p.completeMember()
	case '"':
		p.mode = passthroughJSONString
		p.stringLength = 0
		p.stringTooLong = false
		p.stringEscaped = false
		p.stringEscape = false
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		p.mode = passthroughJSONNumber
		p.number = 0
		p.numberDigits = false
		p.numberValid = current != '-'
		if current >= '0' && current <= '9' {
			p.appendNumberDigit(current)
		}
	default:
		p.mode = passthroughJSONLiteral
	}
}

func (p *passthroughUsageJSONParser) consumeStringByte(current byte) {
	if p.stringEscape {
		p.stringEscape = false
		p.stringEscaped = true
		return
	}
	if current == '\\' {
		p.stringEscape = true
		p.stringEscaped = true
		return
	}
	if current == '"' {
		p.consumeStringToken()
		p.mode = passthroughJSONNormal
		return
	}
	if p.stringLength < len(p.stringBuffer) {
		p.stringBuffer[p.stringLength] = current
	} else {
		p.stringTooLong = true
	}
	p.stringLength++
}

func (p *passthroughUsageJSONParser) consumeStringToken() {
	if p.depth == 0 || p.overflowDepth > 0 {
		return
	}
	frame := &p.frames[p.depth-1]
	if frame.container == passthroughJSONObject && frame.expectingKey {
		frame.currentKey = p.currentStringKey()
		frame.expectingKey = false
		return
	}
	if p.currentValueIsRootType() && !p.stringEscaped && !p.stringTooLong &&
		bytes.Equal(p.stringBuffer[:p.stringLength], []byte(passthroughTerminalResponseEvent)) {
		p.terminal = true
	}
	p.completeValue()
}

func (p *passthroughUsageJSONParser) currentStringKey() passthroughUsageKey {
	if p.stringEscaped || p.stringTooLong {
		return passthroughUsageKeyNone
	}
	return passthroughUsageKeyFor(p.stringBuffer[:p.stringLength])
}

func passthroughUsageKeyFor(value []byte) passthroughUsageKey {
	switch {
	case bytes.Equal(value, []byte("response")):
		return passthroughUsageKeyResponse
	case bytes.Equal(value, []byte("usage")):
		return passthroughUsageKeyUsage
	case bytes.Equal(value, []byte("input_tokens")):
		return passthroughUsageKeyInputTokens
	case bytes.Equal(value, []byte("output_tokens")):
		return passthroughUsageKeyOutputTokens
	case bytes.Equal(value, []byte("total_tokens")):
		return passthroughUsageKeyTotalTokens
	case bytes.Equal(value, []byte("input_tokens_details")):
		return passthroughUsageKeyInputTokensDetails
	case bytes.Equal(value, []byte("cached_tokens")):
		return passthroughUsageKeyCachedTokens
	case bytes.Equal(value, []byte("type")):
		return passthroughUsageKeyType
	default:
		return passthroughUsageKeyNone
	}
}

func (p *passthroughUsageJSONParser) consumeNumberByte(current byte) {
	if current >= '0' && current <= '9' {
		p.appendNumberDigit(current)
		return
	}
	if current == '.' || current == 'e' || current == 'E' || current == '+' || current == '-' {
		p.numberValid = false
		return
	}
	if isPassthroughJSONDelimiter(current) {
		p.consumeNumberToken()
		p.mode = passthroughJSONNormal
		p.consumeNormalByte(current)
		return
	}
	p.numberValid = false
}

func (p *passthroughUsageJSONParser) appendNumberDigit(current byte) {
	digit := int(current - '0')
	maxInt := int(^uint(0) >> 1)
	if p.number > (maxInt-digit)/10 {
		p.numberValid = false
		return
	}
	p.number = p.number*10 + digit
	p.numberDigits = true
}

func (p *passthroughUsageJSONParser) consumeNumberToken() {
	if p.numberValid && p.numberDigits {
		p.recordUsageNumber(p.number)
	}
	p.completeValue()
}

func (p *passthroughUsageJSONParser) consumeLiteralByte(current byte) {
	if !isPassthroughJSONDelimiter(current) {
		return
	}
	p.mode = passthroughJSONNormal
	p.completeValue()
	p.consumeNormalByte(current)
}

func isPassthroughJSONDelimiter(current byte) bool {
	switch current {
	case ' ', '\t', '\n', '\r', ',', '}', ']':
		return true
	default:
		return false
	}
}

func (p *passthroughUsageJSONParser) enterContainer(container passthroughJSONContainer) {
	if p.overflowDepth > 0 {
		p.overflowDepth++
		return
	}
	var pathKey passthroughUsageKey
	if p.depth > 0 {
		frame := &p.frames[p.depth-1]
		if frame.container == passthroughJSONObject {
			pathKey = frame.currentKey
			frame.currentKey = passthroughUsageKeyNone
		}
	}
	if p.depth == len(p.frames) {
		p.overflowDepth = 1
		return
	}
	p.frames[p.depth] = passthroughJSONFrame{
		container: container, pathKey: pathKey, currentKey: passthroughUsageKeyNone,
		expectingKey: container == passthroughJSONObject,
	}
	p.depth++
}

func (p *passthroughUsageJSONParser) leaveContainer() {
	if p.overflowDepth > 0 {
		p.overflowDepth--
		return
	}
	if p.depth == 0 {
		return
	}
	p.depth--
	p.completeValue()
}

func (p *passthroughUsageJSONParser) completeMember() {
	if p.depth == 0 || p.overflowDepth > 0 {
		return
	}
	frame := &p.frames[p.depth-1]
	if frame.container == passthroughJSONObject {
		frame.currentKey = passthroughUsageKeyNone
		frame.expectingKey = true
	}
}

func (p *passthroughUsageJSONParser) completeValue() {
	if p.depth == 0 || p.overflowDepth > 0 {
		return
	}
	frame := &p.frames[p.depth-1]
	if frame.container == passthroughJSONObject {
		frame.currentKey = passthroughUsageKeyNone
	}
}

func (p *passthroughUsageJSONParser) currentValueIsRootType() bool {
	if p.depth != 1 || p.overflowDepth > 0 {
		return false
	}
	frame := p.frames[0]
	return frame.container == passthroughJSONObject && frame.currentKey == passthroughUsageKeyType
}

func (p *passthroughUsageJSONParser) recordUsageNumber(value int) {
	if p.depth == 0 || p.overflowDepth > 0 {
		return
	}
	frame := p.frames[p.depth-1]
	if frame.container != passthroughJSONObject {
		return
	}
	switch {
	case p.matchesUsageField(passthroughUsageKeyInputTokens):
		p.usage.InputTokens = value
		p.usage.PromptTokens = value
	case p.matchesUsageField(passthroughUsageKeyOutputTokens):
		p.usage.OutputTokens = value
		p.usage.CompletionTokens = value
	case p.matchesUsageField(passthroughUsageKeyTotalTokens):
		p.usage.TotalTokens = value
	case p.matchesCachedTokensField():
		p.usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: value}
	}
}

func (p *passthroughUsageJSONParser) matchesUsageField(field passthroughUsageKey) bool {
	if p.depth < 3 {
		return false
	}
	frame := p.frames[p.depth-1]
	parent := p.frames[p.depth-2]
	return frame.currentKey == field && frame.pathKey == passthroughUsageKeyUsage &&
		parent.pathKey == passthroughUsageKeyResponse
}

func (p *passthroughUsageJSONParser) matchesCachedTokensField() bool {
	if p.depth < 4 {
		return false
	}
	frame := p.frames[p.depth-1]
	parent := p.frames[p.depth-2]
	grandparent := p.frames[p.depth-3]
	return frame.currentKey == passthroughUsageKeyCachedTokens &&
		frame.pathKey == passthroughUsageKeyInputTokensDetails &&
		parent.pathKey == passthroughUsageKeyUsage && grandparent.pathKey == passthroughUsageKeyResponse
}

func (p *passthroughUsageJSONParser) Finish() {
	switch p.mode {
	case passthroughJSONNumber:
		p.consumeNumberToken()
	case passthroughJSONLiteral:
		p.completeValue()
	case passthroughJSONNormal, passthroughJSONString:
	}
	p.mode = passthroughJSONNormal
}

func (p *passthroughUsageJSONParser) Reset() {
	var frames [passthroughMaxJSONDepth]passthroughJSONFrame
	var usage Usage
	p.frames = frames
	p.depth = 0
	p.overflowDepth = 0
	p.mode = passthroughJSONNormal
	p.stringLength = 0
	p.stringTooLong = false
	p.stringEscaped = false
	p.stringEscape = false
	p.number = 0
	p.numberDigits = false
	p.numberValid = false
	p.terminal = false
	p.usage = usage
	p.contentSeen = false
}

func (p *passthroughUsageJSONParser) hasContent() bool {
	return p.contentSeen
}
