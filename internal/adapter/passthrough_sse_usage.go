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
		p.eventTerminal = p.eventNameMatches && p.eventNameLength == len(passthroughTerminalResponseEvent)
	case passthroughSSELineUnknown:
	}
	p.resetLine()
}

func (p *passthroughSSEUsageParser) finishEvent() {
	p.json.Finish()
	if p.eventTerminal && p.json.terminal && p.json.Valid() {
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

type passthroughJSONObjectState uint8

const (
	passthroughJSONObjectKeyOrEnd passthroughJSONObjectState = iota
	passthroughJSONObjectKey
	passthroughJSONObjectColon
	passthroughJSONObjectValue
	passthroughJSONObjectCommaOrEnd
)

type passthroughJSONArrayState uint8

const (
	passthroughJSONArrayValueOrEnd passthroughJSONArrayState = iota
	passthroughJSONArrayValue
	passthroughJSONArrayCommaOrEnd
)

type passthroughJSONRootState uint8

const (
	passthroughJSONRootValue passthroughJSONRootState = iota
	passthroughJSONRootComplete
)

type passthroughJSONNumberState uint8

const (
	passthroughJSONNumberMinus passthroughJSONNumberState = iota
	passthroughJSONNumberZero
	passthroughJSONNumberInteger
	passthroughJSONNumberFractionStart
	passthroughJSONNumberFraction
	passthroughJSONNumberExponentStart
	passthroughJSONNumberExponentSign
	passthroughJSONNumberExponent
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
	container   passthroughJSONContainer
	pathKey     passthroughUsageKey
	currentKey  passthroughUsageKey
	objectState passthroughJSONObjectState
	arrayState  passthroughJSONArrayState
}

type passthroughUsageJSONParser struct {
	frames              [passthroughMaxJSONDepth]passthroughJSONFrame
	depth               int
	overflowDepth       int
	mode                passthroughJSONMode
	rootState           passthroughJSONRootState
	numberState         passthroughJSONNumberState
	number              int
	numberFits          bool
	numberNegative      bool
	stringBuffer        [passthroughMaxJSONStringBytes]byte
	stringLength        int
	stringTooLong       bool
	stringEscaped       bool
	stringEscape        bool
	stringUnicodeDigits int
	stringIsKey         bool
	literalBuffer       [5]byte
	literalLength       int
	invalid             bool
	terminal            bool
	usage               Usage
	contentSeen         bool
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
	if isPassthroughJSONWhitespace(current) {
		return
	}
	switch current {
	case '{':
		p.enterContainer(passthroughJSONObject)
	case '[':
		p.enterContainer(passthroughJSONArray)
	case '}', ']':
		p.leaveContainer(current)
	case ',':
		p.consumeComma()
	case ':':
		p.consumeColon()
	case '"':
		p.beginString()
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		p.beginNumber(current)
	default:
		p.beginLiteral(current)
	}
}

func isPassthroughJSONWhitespace(current byte) bool {
	switch current {
	case ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

func (p *passthroughUsageJSONParser) beginString() {
	p.stringLength = 0
	p.stringTooLong = false
	p.stringEscaped = false
	p.stringEscape = false
	p.stringUnicodeDigits = 0
	p.stringIsKey = p.expectsObjectKey()
	if !p.stringIsKey && !p.beginValue() {
		p.invalid = true
	}
	p.mode = passthroughJSONString
}

func (p *passthroughUsageJSONParser) consumeStringByte(current byte) {
	if p.stringUnicodeDigits > 0 {
		if !isPassthroughJSONHex(current) {
			p.invalid = true
		}
		p.stringUnicodeDigits--
		return
	}
	if p.stringEscape {
		p.stringEscape = false
		p.stringEscaped = true
		if current == 'u' {
			p.stringUnicodeDigits = 4
			return
		}
		if !isPassthroughJSONEscape(current) {
			p.invalid = true
		}
		return
	}
	if current == '\\' {
		p.stringEscape = true
		p.stringEscaped = true
		return
	}
	if current == '"' {
		p.finishString()
		p.mode = passthroughJSONNormal
		return
	}
	if current < 0x20 {
		p.invalid = true
		return
	}
	p.appendStringByte(current)
}

func isPassthroughJSONHex(current byte) bool {
	return (current >= '0' && current <= '9') || (current >= 'a' && current <= 'f') || (current >= 'A' && current <= 'F')
}

func isPassthroughJSONEscape(current byte) bool {
	switch current {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return true
	default:
		return false
	}
}

func (p *passthroughUsageJSONParser) appendStringByte(current byte) {
	if p.stringLength < len(p.stringBuffer) {
		p.stringBuffer[p.stringLength] = current
	} else {
		p.stringTooLong = true
	}
	p.stringLength++
}

func (p *passthroughUsageJSONParser) finishString() {
	if p.stringEscape || p.stringUnicodeDigits != 0 {
		p.invalid = true
	}
	if p.stringIsKey {
		p.setObjectKey()
		return
	}
	if p.currentValueIsRootType() && !p.stringEscaped && !p.stringTooLong &&
		bytes.Equal(p.stringBuffer[:p.stringLength], []byte(passthroughTerminalResponseEvent)) {
		p.terminal = true
	}
	p.completeScalar()
}

func (p *passthroughUsageJSONParser) beginNumber(current byte) {
	if !p.beginValue() {
		p.invalid = true
	}
	p.number = 0
	p.numberFits = true
	p.numberNegative = current == '-'
	switch current {
	case '-':
		p.numberState = passthroughJSONNumberMinus
	case '0':
		p.numberState = passthroughJSONNumberZero
		p.appendNumberDigit(current)
	default:
		p.numberState = passthroughJSONNumberInteger
		p.appendNumberDigit(current)
	}
	p.mode = passthroughJSONNumber
}

func (p *passthroughUsageJSONParser) consumeNumberByte(current byte) {
	if isPassthroughJSONValueDelimiter(current) {
		p.finishNumber()
		p.mode = passthroughJSONNormal
		p.consumeNormalByte(current)
		return
	}
	switch p.numberState {
	case passthroughJSONNumberMinus:
		p.consumeNumberAfterMinus(current)
	case passthroughJSONNumberZero:
		p.consumeNumberAfterZero(current)
	case passthroughJSONNumberInteger:
		p.consumeNumberAfterInteger(current)
	case passthroughJSONNumberFractionStart:
		p.consumeNumberAfterFractionStart(current)
	case passthroughJSONNumberFraction:
		p.consumeNumberAfterFraction(current)
	case passthroughJSONNumberExponentStart:
		p.consumeNumberAfterExponentStart(current)
	case passthroughJSONNumberExponentSign:
		p.consumeNumberAfterExponentSign(current)
	case passthroughJSONNumberExponent:
		p.consumeNumberAfterExponent(current)
	}
}

func (p *passthroughUsageJSONParser) consumeNumberAfterMinus(current byte) {
	switch {
	case current == '0':
		p.numberState = passthroughJSONNumberZero
		p.appendNumberDigit(current)
	case current >= '1' && current <= '9':
		p.numberState = passthroughJSONNumberInteger
		p.appendNumberDigit(current)
	default:
		p.invalid = true
	}
}

func (p *passthroughUsageJSONParser) consumeNumberAfterZero(current byte) {
	switch current {
	case '.':
		p.numberState = passthroughJSONNumberFractionStart
	case 'e', 'E':
		p.numberState = passthroughJSONNumberExponentStart
	default:
		p.invalid = true
	}
}

func (p *passthroughUsageJSONParser) consumeNumberAfterInteger(current byte) {
	switch {
	case current >= '0' && current <= '9':
		p.appendNumberDigit(current)
	case current == '.':
		p.numberState = passthroughJSONNumberFractionStart
	case current == 'e' || current == 'E':
		p.numberState = passthroughJSONNumberExponentStart
	default:
		p.invalid = true
	}
}

func (p *passthroughUsageJSONParser) consumeNumberAfterFractionStart(current byte) {
	if current >= '0' && current <= '9' {
		p.numberState = passthroughJSONNumberFraction
		return
	}
	p.invalid = true
}

func (p *passthroughUsageJSONParser) consumeNumberAfterFraction(current byte) {
	switch {
	case current >= '0' && current <= '9':
	case current == 'e' || current == 'E':
		p.numberState = passthroughJSONNumberExponentStart
	default:
		p.invalid = true
	}
}

func (p *passthroughUsageJSONParser) consumeNumberAfterExponentStart(current byte) {
	switch {
	case current == '+' || current == '-':
		p.numberState = passthroughJSONNumberExponentSign
	case current >= '0' && current <= '9':
		p.numberState = passthroughJSONNumberExponent
	default:
		p.invalid = true
	}
}

func (p *passthroughUsageJSONParser) consumeNumberAfterExponentSign(current byte) {
	if current >= '0' && current <= '9' {
		p.numberState = passthroughJSONNumberExponent
		return
	}
	p.invalid = true
}

func (p *passthroughUsageJSONParser) consumeNumberAfterExponent(current byte) {
	if current < '0' || current > '9' {
		p.invalid = true
	}
}

func isPassthroughJSONValueDelimiter(current byte) bool {
	return isPassthroughJSONWhitespace(current) || current == ',' || current == '}' || current == ']'
}

func (p *passthroughUsageJSONParser) appendNumberDigit(current byte) {
	digit := int(current - '0')
	maxInt := int(^uint(0) >> 1)
	if p.number > (maxInt-digit)/10 {
		p.numberFits = false
		return
	}
	p.number = p.number*10 + digit
}

func (p *passthroughUsageJSONParser) finishNumber() {
	if !p.numberIsComplete() {
		p.invalid = true
	}
	if p.numberNegative && p.isRootUsageCounter() {
		p.invalid = true
	}
	if p.numberIsInteger() && p.numberFits && !p.numberNegative {
		p.recordUsageNumber(p.number)
	}
	p.completeScalar()
}

func (p *passthroughUsageJSONParser) isRootUsageCounter() bool {
	if p.depth == 3 && p.overflowDepth == 0 {
		root := p.frames[0]
		response := p.frames[1]
		usage := p.frames[2]
		return root.container == passthroughJSONObject && root.pathKey == passthroughUsageKeyNone &&
			response.container == passthroughJSONObject && response.pathKey == passthroughUsageKeyResponse &&
			usage.container == passthroughJSONObject && usage.pathKey == passthroughUsageKeyUsage &&
			(usage.currentKey == passthroughUsageKeyInputTokens || usage.currentKey == passthroughUsageKeyOutputTokens || usage.currentKey == passthroughUsageKeyTotalTokens)
	}
	if p.depth == 4 && p.overflowDepth == 0 {
		root := p.frames[0]
		response := p.frames[1]
		usage := p.frames[2]
		details := p.frames[3]
		return root.container == passthroughJSONObject && root.pathKey == passthroughUsageKeyNone &&
			response.container == passthroughJSONObject && response.pathKey == passthroughUsageKeyResponse &&
			usage.container == passthroughJSONObject && usage.pathKey == passthroughUsageKeyUsage &&
			details.container == passthroughJSONObject && details.pathKey == passthroughUsageKeyInputTokensDetails &&
			details.currentKey == passthroughUsageKeyCachedTokens
	}
	return false
}

func (p *passthroughUsageJSONParser) numberIsComplete() bool {
	switch p.numberState {
	case passthroughJSONNumberZero, passthroughJSONNumberInteger, passthroughJSONNumberFraction, passthroughJSONNumberExponent:
		return true
	case passthroughJSONNumberMinus, passthroughJSONNumberFractionStart, passthroughJSONNumberExponentStart, passthroughJSONNumberExponentSign:
		return false
	default:
		return false
	}
}

func (p *passthroughUsageJSONParser) numberIsInteger() bool {
	return p.numberState == passthroughJSONNumberZero || p.numberState == passthroughJSONNumberInteger
}

func (p *passthroughUsageJSONParser) beginLiteral(current byte) {
	if !p.beginValue() {
		p.invalid = true
	}
	p.literalLength = 0
	p.appendLiteralByte(current)
	p.mode = passthroughJSONLiteral
}

func (p *passthroughUsageJSONParser) consumeLiteralByte(current byte) {
	if isPassthroughJSONValueDelimiter(current) {
		p.finishLiteral()
		p.mode = passthroughJSONNormal
		p.consumeNormalByte(current)
		return
	}
	p.appendLiteralByte(current)
}

func (p *passthroughUsageJSONParser) appendLiteralByte(current byte) {
	if p.literalLength < len(p.literalBuffer) {
		p.literalBuffer[p.literalLength] = current
	} else {
		p.invalid = true
	}
	p.literalLength++
}

func (p *passthroughUsageJSONParser) finishLiteral() {
	if p.literalLength > len(p.literalBuffer) {
		p.invalid = true
	} else {
		literal := p.literalBuffer[:p.literalLength]
		if !bytes.Equal(literal, []byte("true")) && !bytes.Equal(literal, []byte("false")) && !bytes.Equal(literal, []byte("null")) {
			p.invalid = true
		}
	}
	p.completeScalar()
}

func (p *passthroughUsageJSONParser) enterContainer(container passthroughJSONContainer) {
	if !p.beginValue() {
		p.invalid = true
	}
	if p.overflowDepth > 0 {
		p.overflowDepth++
		return
	}
	var pathKey passthroughUsageKey
	if p.depth > 0 {
		parent := &p.frames[p.depth-1]
		if parent.container == passthroughJSONObject {
			pathKey = parent.currentKey
			parent.currentKey = passthroughUsageKeyNone
		}
	}
	if p.depth == len(p.frames) {
		p.invalid = true
		p.overflowDepth = 1
		return
	}
	p.frames[p.depth] = passthroughJSONFrame{
		container: container, pathKey: pathKey, currentKey: passthroughUsageKeyNone,
		objectState: passthroughJSONObjectKeyOrEnd, arrayState: passthroughJSONArrayValueOrEnd,
	}
	p.depth++
}

func (p *passthroughUsageJSONParser) leaveContainer(current byte) {
	if p.overflowDepth > 0 {
		p.overflowDepth--
		return
	}
	if p.depth == 0 {
		p.invalid = true
		return
	}
	frame := p.frames[p.depth-1]
	if (current == '}' && frame.container != passthroughJSONObject) || (current == ']' && frame.container != passthroughJSONArray) {
		p.invalid = true
		return
	}
	if !frame.canClose() {
		p.invalid = true
		return
	}
	p.depth--
}

func (f passthroughJSONFrame) canClose() bool {
	if f.container == passthroughJSONObject {
		return f.objectState == passthroughJSONObjectKeyOrEnd || f.objectState == passthroughJSONObjectCommaOrEnd
	}
	return f.arrayState == passthroughJSONArrayValueOrEnd || f.arrayState == passthroughJSONArrayCommaOrEnd
}

func (p *passthroughUsageJSONParser) consumeComma() {
	if p.depth == 0 || p.overflowDepth > 0 {
		p.invalid = true
		return
	}
	frame := &p.frames[p.depth-1]
	if frame.container == passthroughJSONObject {
		if frame.objectState != passthroughJSONObjectCommaOrEnd {
			p.invalid = true
			return
		}
		frame.objectState = passthroughJSONObjectKey
		return
	}
	if frame.arrayState != passthroughJSONArrayCommaOrEnd {
		p.invalid = true
		return
	}
	frame.arrayState = passthroughJSONArrayValue
}

func (p *passthroughUsageJSONParser) consumeColon() {
	if p.depth == 0 || p.overflowDepth > 0 {
		p.invalid = true
		return
	}
	frame := &p.frames[p.depth-1]
	if frame.container != passthroughJSONObject || frame.objectState != passthroughJSONObjectColon {
		p.invalid = true
		return
	}
	frame.objectState = passthroughJSONObjectValue
}

func (p *passthroughUsageJSONParser) expectsObjectKey() bool {
	if p.depth == 0 || p.overflowDepth > 0 {
		return false
	}
	frame := p.frames[p.depth-1]
	return frame.container == passthroughJSONObject &&
		(frame.objectState == passthroughJSONObjectKeyOrEnd || frame.objectState == passthroughJSONObjectKey)
}

func (p *passthroughUsageJSONParser) setObjectKey() {
	if !p.expectsObjectKey() {
		p.invalid = true
		return
	}
	frame := &p.frames[p.depth-1]
	frame.currentKey = p.currentStringKey()
	frame.objectState = passthroughJSONObjectColon
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

func (p *passthroughUsageJSONParser) beginValue() bool {
	if p.depth == 0 {
		if p.rootState != passthroughJSONRootValue {
			return false
		}
		p.rootState = passthroughJSONRootComplete
		return true
	}
	if p.overflowDepth > 0 {
		return true
	}
	frame := &p.frames[p.depth-1]
	if frame.container == passthroughJSONObject && frame.objectState == passthroughJSONObjectValue {
		frame.objectState = passthroughJSONObjectCommaOrEnd
		return true
	}
	if frame.container == passthroughJSONArray &&
		(frame.arrayState == passthroughJSONArrayValueOrEnd || frame.arrayState == passthroughJSONArrayValue) {
		frame.arrayState = passthroughJSONArrayCommaOrEnd
		return true
	}
	return false
}

func (p *passthroughUsageJSONParser) completeScalar() {
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
	if p.depth == 3 && p.overflowDepth == 0 {
		root := p.frames[0]
		response := p.frames[1]
		usage := p.frames[2]
		if root.container == passthroughJSONObject && root.pathKey == passthroughUsageKeyNone &&
			response.container == passthroughJSONObject && response.pathKey == passthroughUsageKeyResponse &&
			usage.container == passthroughJSONObject && usage.pathKey == passthroughUsageKeyUsage {
			switch usage.currentKey {
			case passthroughUsageKeyInputTokens:
				p.usage.InputTokens = value
				p.usage.PromptTokens = value
			case passthroughUsageKeyOutputTokens:
				p.usage.OutputTokens = value
				p.usage.CompletionTokens = value
			case passthroughUsageKeyTotalTokens:
				p.usage.TotalTokens = value
			case passthroughUsageKeyNone, passthroughUsageKeyResponse, passthroughUsageKeyUsage,
				passthroughUsageKeyInputTokensDetails, passthroughUsageKeyCachedTokens, passthroughUsageKeyType:
			default:
			}
		}
	}
	if p.depth == 4 && p.overflowDepth == 0 {
		root := p.frames[0]
		response := p.frames[1]
		usage := p.frames[2]
		details := p.frames[3]
		if root.container == passthroughJSONObject && root.pathKey == passthroughUsageKeyNone &&
			response.container == passthroughJSONObject && response.pathKey == passthroughUsageKeyResponse &&
			usage.container == passthroughJSONObject && usage.pathKey == passthroughUsageKeyUsage &&
			details.container == passthroughJSONObject && details.pathKey == passthroughUsageKeyInputTokensDetails &&
			details.currentKey == passthroughUsageKeyCachedTokens {
			p.usage.PromptTokensDetails = &PromptTokensDetails{CachedTokens: value}
		}
	}
}

func (p *passthroughUsageJSONParser) Finish() {
	switch p.mode {
	case passthroughJSONString:
		p.invalid = true
	case passthroughJSONNumber:
		p.finishNumber()
	case passthroughJSONLiteral:
		p.finishLiteral()
	case passthroughJSONNormal:
	}
	p.mode = passthroughJSONNormal
}

func (p *passthroughUsageJSONParser) Valid() bool {
	return !p.invalid && p.overflowDepth == 0 && p.depth == 0 && p.rootState == passthroughJSONRootComplete
}

func (p *passthroughUsageJSONParser) Reset() {
	var frames [passthroughMaxJSONDepth]passthroughJSONFrame
	var usage Usage
	p.frames = frames
	p.depth = 0
	p.overflowDepth = 0
	p.mode = passthroughJSONNormal
	p.rootState = passthroughJSONRootValue
	p.numberState = passthroughJSONNumberMinus
	p.number = 0
	p.numberFits = false
	p.numberNegative = false
	p.stringLength = 0
	p.stringTooLong = false
	p.stringEscaped = false
	p.stringEscape = false
	p.stringUnicodeDigits = 0
	p.stringIsKey = false
	p.literalLength = 0
	p.invalid = false
	p.terminal = false
	p.usage = usage
	p.contentSeen = false
}

func (p *passthroughUsageJSONParser) hasContent() bool {
	return p.contentSeen
}
