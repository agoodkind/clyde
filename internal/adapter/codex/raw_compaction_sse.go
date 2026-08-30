package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
)

type rawCompactionSSEEvent string

const (
	maxRawCompactionSSEPendingBytes                             = 8 * 1024 * 1024
	rawCompactionSSEPassthroughChunkBytes                       = 32 * 1024
	rawCompactionSSEOutputItemDone        rawCompactionSSEEvent = "response.output_item.done"
	rawCompactionSSEContentPartAdded      rawCompactionSSEEvent = "response.content_part.added"
	rawCompactionSSEContentPartDone       rawCompactionSSEEvent = "response.content_part.done"
	rawCompactionSSEOutputTextDelta       rawCompactionSSEEvent = "response.output_text.delta"
	rawCompactionSSEOutputTextDone        rawCompactionSSEEvent = "response.output_text.done"
	rawCompactionSSECompleted             rawCompactionSSEEvent = "response.completed"
	rawCompactionSSEFailed                rawCompactionSSEEvent = "response.failed"
	rawCompactionSSEIncomplete            rawCompactionSSEEvent = "response.incomplete"
	rawCompactionSSEError                 rawCompactionSSEEvent = "error"
)

type rawCompactionSSEBody struct {
	inner      io.ReadCloser
	reader     *bufio.Reader
	transcript string
	pending    []byte
	pendingErr error
	candidate  []byte
	following  []byte
	disabled   bool
}

// RequiresTerminalValidation reports whether streamed mutation state must be
// validated before accepting the response.
func (t *RawResponsesCompactionTransformer) RequiresTerminalValidation() bool {
	return false
}

func newRawCompactionSSEBody(inner io.ReadCloser, transcriptText string) *rawCompactionSSEBody {
	return &rawCompactionSSEBody{
		inner:      inner,
		reader:     bufio.NewReader(inner),
		transcript: transcriptText,
		pending:    nil,
		pendingErr: nil,
		candidate:  nil,
		following:  nil,
		disabled:   false,
	}
}

func (b *rawCompactionSSEBody) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	for len(b.pending) == 0 {
		if b.pendingErr != nil {
			err := b.pendingErr
			b.pendingErr = nil
			return 0, err
		}
		if err := b.loadNextFrame(); err != nil {
			return 0, err
		}
	}
	count := copy(destination, b.pending)
	b.pending = b.pending[count:]
	return count, nil
}

func (b *rawCompactionSSEBody) loadNextFrame() error {
	if b.disabled {
		return b.loadPassthroughBytes()
	}
	frame, readErr, oversized := readRawCompactionSSEFrame(
		b.reader,
		maxRawCompactionSSEPendingBytes,
	)
	if len(frame) == 0 {
		return b.flushCandidateAtEOF(readErr)
	}
	if oversized {
		return b.failOpenSSE(frame, readErr)
	}
	switch rawSSEFrameEvent(frame) {
	case rawCompactionSSEOutputItemDone:
		return b.handleSSECandidateFrame(frame, readErr)
	case rawCompactionSSECompleted:
		return b.handleSSECompletedFrame(frame, readErr)
	case rawCompactionSSEFailed,
		rawCompactionSSEIncomplete,
		rawCompactionSSEError:
		return b.failOpenSSE(frame, readErr)
	case rawCompactionSSEContentPartAdded,
		rawCompactionSSEContentPartDone,
		rawCompactionSSEOutputTextDelta,
		rawCompactionSSEOutputTextDone:
		return b.handleSSEOtherFrame(frame, readErr)
	default:
		return b.handleSSEOtherFrame(frame, readErr)
	}
}

func (b *rawCompactionSSEBody) loadPassthroughBytes() error {
	chunk := make([]byte, rawCompactionSSEPassthroughChunkBytes)
	count, readErr := b.reader.Read(chunk)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		readErr = fmt.Errorf("read raw compaction SSE passthrough: %w", readErr)
	}
	if count == 0 {
		return readErr
	}
	b.pending = chunk[:count]
	return b.queueSSEError(readErr)
}

func (b *rawCompactionSSEBody) handleSSEOtherFrame(frame []byte, readErr error) error {
	if len(b.candidate) > 0 {
		if readErr != nil || !rawCompactionUnknownSSEFrameIsValid(frame) {
			return b.failOpenSSE(frame, readErr)
		}
		if !rawCompactionSSEPendingFits(b.candidate, b.following, frame) {
			return b.failOpenSSE(frame, readErr)
		}
		b.following = append(b.following, frame...)
		return nil
	}
	return b.queueSSEBytes(frame, readErr)
}

func (b *rawCompactionSSEBody) handleSSECandidateFrame(frame []byte, readErr error) error {
	if !rawCompactionSSEJSONFrameIsValid(frame, rawCompactionSSEOutputItemDone) {
		return b.failOpenSSE(frame, readErr)
	}
	_, matched, valid := appendRawCompactionSSEFrame(frame, b.transcript)
	if !valid {
		return b.failOpenSSE(frame, readErr)
	}
	if !matched {
		if len(b.candidate) > 0 {
			return b.failOpenSSE(frame, readErr)
		}
		return b.queueSSEBytes(frame, readErr)
	}
	if readErr != nil {
		return b.failOpenSSE(frame, readErr)
	}
	if len(frame) > maxRawCompactionSSEPendingBytes {
		return b.failOpenSSE(frame, readErr)
	}
	if len(b.candidate) > 0 {
		b.pending = joinRawCompactionSSEFrames(b.candidate, b.following)
	}
	b.candidate = frame
	b.following = nil
	return nil
}

func rawCompactionSSEPendingFits(candidate []byte, following []byte, frame []byte) bool {
	retained := len(candidate) + len(following)
	return retained <= maxRawCompactionSSEPendingBytes && len(frame) <= maxRawCompactionSSEPendingBytes-retained
}

func (b *rawCompactionSSEBody) handleSSECompletedFrame(frame []byte, readErr error) error {
	if readErr != nil {
		return b.failOpenSSE(frame, readErr)
	}
	if !rawCompactionSSEJSONFrameIsValid(frame, rawCompactionSSECompleted) {
		return b.failOpenSSE(frame, readErr)
	}
	if len(b.candidate) == 0 {
		b.pending = frame
		return b.queueSSEError(readErr)
	}
	mutatedFrames, ok := appendRawCompactionSSEStreamEvents(b.candidate, b.following, frame, b.transcript)
	if !ok {
		return b.failOpenSSE(frame, readErr)
	}
	b.pending = mutatedFrames
	b.candidate = nil
	b.following = nil
	return b.queueSSEError(readErr)
}

func (b *rawCompactionSSEBody) flushCandidateAtEOF(readErr error) error {
	if len(b.candidate) == 0 {
		return readErr
	}
	b.pending = joinRawCompactionSSEFrames(b.candidate, b.following)
	b.candidate = nil
	b.following = nil
	b.disabled = true
	return b.queueSSEError(readErr)
}

func (b *rawCompactionSSEBody) failOpenSSE(frame []byte, readErr error) error {
	b.pending = joinRawCompactionSSEFrames(
		joinRawCompactionSSEFrames(b.candidate, b.following),
		frame,
	)
	b.candidate = nil
	b.following = nil
	b.disabled = true
	return b.queueSSEError(readErr)
}

func (b *rawCompactionSSEBody) queueSSEBytes(frame []byte, readErr error) error {
	b.pending = frame
	return b.queueSSEError(readErr)
}

func (b *rawCompactionSSEBody) queueSSEError(readErr error) error {
	b.pendingErr = readErr
	return nil
}

func joinRawCompactionSSEFrames(first, second []byte) []byte {
	joined := make([]byte, 0, len(first)+len(second))
	joined = append(joined, first...)
	joined = append(joined, second...)
	return joined
}

func rawCompactionSSEJSONFrameIsValid(frame []byte, eventName rawCompactionSSEEvent) bool {
	gotEvent, data, dataCount := rawSSEFrameDataValue(frame)
	if gotEvent != string(eventName) || dataCount == 0 || !json.Valid(data) {
		return false
	}
	var payload struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(data, &payload) == nil && payload.Type == string(eventName)
}

func rawCompactionUnknownSSEFrameIsValid(frame []byte) bool {
	_, dataStart, dataEnd, dataCount := rawSSEFrameData(frame)
	if dataCount == 0 {
		return true
	}
	return dataCount == 1 && json.Valid(frame[dataStart:dataEnd])
}

func (b *rawCompactionSSEBody) Close() error {
	if err := b.inner.Close(); err != nil {
		slog.Warn("adapter.codex.raw_compaction.sse_close_failed", "concern", "adapter.providers.codex.request", "err", err)
		return fmt.Errorf("close raw compaction SSE response: %w", err)
	}
	return nil
}

func readRawCompactionSSEFrame(reader *bufio.Reader, maxBytes int) ([]byte, error, bool) {
	var frame bytes.Buffer
	lineStart := 0
	for {
		remaining := maxBytes - frame.Len()
		if remaining < reader.Size() {
			result := readRawCompactionSSEFrameByte(
				reader,
				&frame,
				lineStart,
				maxBytes,
			)
			lineStart = result.lineStart
			if result.complete || result.oversized || result.readErr != nil {
				return frame.Bytes(), result.readErr, result.oversized
			}
			continue
		}
		line, err := reader.ReadSlice('\n')
		frame.Write(line)
		if frame.Len() > maxBytes {
			if errors.Is(err, bufio.ErrBufferFull) {
				err = nil
			}
			return frame.Bytes(), err, true
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			logicalLine := frame.Bytes()[lineStart:]
			if rawCompactionSSEBlankLine(logicalLine) {
				return frame.Bytes(), err, false
			}
			lineStart = frame.Len()
		}
		if err != nil {
			return frame.Bytes(), err, false
		}
	}
}

type rawCompactionSSEFrameByteResult struct {
	lineStart int
	complete  bool
	oversized bool
	readErr   error
}

func readRawCompactionSSEFrameByte(
	reader *bufio.Reader,
	frame *bytes.Buffer,
	lineStart int,
	maxBytes int,
) rawCompactionSSEFrameByteResult {
	value, err := reader.ReadByte()
	if err != nil {
		return rawCompactionSSEFrameByteResult{
			lineStart: lineStart,
			complete:  false,
			oversized: false,
			readErr:   err,
		}
	}
	frame.WriteByte(value)
	if frame.Len() > maxBytes {
		return rawCompactionSSEFrameByteResult{
			lineStart: lineStart,
			complete:  false,
			oversized: true,
			readErr:   nil,
		}
	}
	if value != '\n' {
		return rawCompactionSSEFrameByteResult{
			lineStart: lineStart,
			complete:  false,
			oversized: false,
			readErr:   nil,
		}
	}
	if rawCompactionSSEBlankLine(frame.Bytes()[lineStart:]) {
		return rawCompactionSSEFrameByteResult{
			lineStart: lineStart,
			complete:  true,
			oversized: false,
			readErr:   nil,
		}
	}
	return rawCompactionSSEFrameByteResult{
		lineStart: frame.Len(),
		complete:  false,
		oversized: false,
		readErr:   nil,
	}
}

func rawCompactionSSEBlankLine(line []byte) bool {
	return bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))
}

func appendRawCompactionSSEFrame(frame []byte, transcriptText string) ([]byte, bool, bool) {
	eventName, data, dataCount := rawSSEFrameDataValue(frame)
	if eventName != string(rawCompactionSSEOutputItemDone) {
		return frame, false, true
	}
	if dataCount == 0 {
		return frame, false, false
	}
	itemStart, itemEnd, ok := jsonObjectFieldValueRange(data, "item")
	if !ok {
		return frame, false, false
	}
	mutated, matched, valid := appendRawCompactionAssistantItem(data[itemStart:itemEnd], transcriptText)
	if !valid || !matched {
		return frame, matched, valid
	}
	if bytes.Equal(mutated, data[itemStart:itemEnd]) {
		return frame, true, true
	}
	return replaceRawSSEFrameData(frame, replaceByteRange(data, itemStart, itemEnd, mutated)), true, true
}

const rawCompactionSSESyntheticEventCount = 4

type rawCompactionSSEContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rawCompactionSSESyntheticContentPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
}

type rawCompactionSSESyntheticEvent struct {
	Type           string                                `json:"type"`
	ItemID         string                                `json:"item_id"`
	OutputIndex    int                                   `json:"output_index"`
	ContentIndex   int                                   `json:"content_index"`
	Part           *rawCompactionSSESyntheticContentPart `json:"part,omitempty"`
	Delta          string                                `json:"delta,omitempty"`
	Text           string                                `json:"text,omitempty"`
	Logprobs       *[]json.RawMessage                    `json:"logprobs,omitempty"`
	SequenceNumber int                                   `json:"sequence_number"`
}

type rawCompactionSSEItemIdentity struct {
	id           string
	outputIndex  int
	contentIndex int
	sequence     int
}

func appendRawCompactionSSEStreamEvents(candidate, following, completed []byte, transcriptText string) ([]byte, bool) {
	candidateData, candidateItem, identity, ok := rawCompactionSSECandidateItem(candidate)
	if !ok {
		return nil, false
	}
	completedData, completedItem, completedItemStart, completedItemEnd, ok := rawCompactionSSECompletedItem(completed, identity)
	if !ok || !rawCompactionSSEItemsHaveCoherentContent(candidateItem, completedItem) {
		return nil, false
	}
	mutatedCandidateItem, candidateAppended, candidateValid := appendRawCompactionSSEContentPart(candidateItem, transcriptText)
	mutatedCompletedItem, completedAppended, completedValid := appendRawCompactionSSEContentPart(completedItem, transcriptText)
	if !candidateValid || !completedValid || candidateAppended != completedAppended {
		return nil, false
	}
	if !candidateAppended {
		return joinRawCompactionSSEFrames(joinRawCompactionSSEFrames(candidate, following), completed), true
	}
	mutatedCandidateSequence, ok := addRawCompactionSSESyntheticEventCount(identity.sequence)
	if !ok {
		return nil, false
	}
	shiftedFollowing, lastSequence, ok := shiftRawCompactionSSEFrames(following, identity.sequence)
	if !ok {
		return nil, false
	}
	completedSequence, ok := rawCompactionSSEIntegerField(completedData, "sequence_number")
	if !ok || completedSequence <= lastSequence {
		return nil, false
	}
	mutatedCompletedSequence, ok := addRawCompactionSSESyntheticEventCount(completedSequence)
	if !ok {
		return nil, false
	}
	mutatedCandidateData := replaceRawCompactionSSEItemAndSequence(candidateData, mutatedCandidateItem, mutatedCandidateSequence)
	mutatedCompletedData := replaceByteRange(completedData, completedItemStart, completedItemEnd, mutatedCompletedItem)
	mutatedCompletedData, ok = replaceRawCompactionSSEIntegerField(mutatedCompletedData, "sequence_number", mutatedCompletedSequence)
	if !ok {
		return nil, false
	}
	synthetic, ok := rawCompactionSSESyntheticContentEvents(identity, transcriptText)
	if !ok {
		return nil, false
	}
	result := joinRawCompactionSSEFrames(synthetic, replaceRawSSEFrameData(candidate, mutatedCandidateData))
	result = joinRawCompactionSSEFrames(result, shiftedFollowing)
	return joinRawCompactionSSEFrames(result, replaceRawSSEFrameData(completed, mutatedCompletedData)), true
}

func rawCompactionSSECandidateItem(frame []byte) ([]byte, []byte, rawCompactionSSEItemIdentity, bool) {
	emptyIdentity := rawCompactionSSEItemIdentity{id: "", outputIndex: 0, contentIndex: 0, sequence: 0}
	_, data, _ := rawSSEFrameDataValue(frame)
	itemStart, itemEnd, hasItem := jsonObjectFieldValueRange(data, "item")
	outputIndex, hasOutputIndex := rawCompactionSSEIntegerField(data, "output_index")
	sequence, hasSequence := rawCompactionSSEIntegerField(data, "sequence_number")
	if !hasItem || !hasOutputIndex || outputIndex < 0 || !hasSequence || sequence < 0 {
		return nil, nil, emptyIdentity, false
	}
	item := data[itemStart:itemEnd]
	itemID, hasItemID := rawCompactionSSEStringField(item, "id")
	contentIndex, contentOK := rawCompactionSSEContentCount(item)
	if !hasItemID || itemID == "" || !contentOK {
		return nil, nil, emptyIdentity, false
	}
	return data, item, rawCompactionSSEItemIdentity{
		id:           itemID,
		outputIndex:  outputIndex,
		contentIndex: contentIndex,
		sequence:     sequence,
	}, true
}

func rawCompactionSSECompletedItem(frame []byte, identity rawCompactionSSEItemIdentity) ([]byte, []byte, int, int, bool) {
	_, data, _ := rawSSEFrameDataValue(frame)
	responseStart, responseEnd, hasResponse := jsonObjectFieldValueRange(data, "response")
	if !hasResponse {
		return nil, nil, 0, 0, false
	}
	response := data[responseStart:responseEnd]
	outputStart, outputEnd, hasOutput := jsonObjectFieldValueRange(response, "output")
	if !hasOutput {
		return nil, nil, 0, 0, false
	}
	outputRanges, validOutput := jsonArrayValueRanges(response[outputStart:outputEnd])
	if !validOutput || identity.outputIndex >= len(outputRanges) {
		return nil, nil, 0, 0, false
	}
	itemRange := outputRanges[identity.outputIndex]
	itemStart := responseStart + outputStart + itemRange.start
	itemEnd := responseStart + outputStart + itemRange.end
	item := data[itemStart:itemEnd]
	itemID, hasItemID := rawCompactionSSEStringField(item, "id")
	if !hasItemID || itemID != identity.id {
		return nil, nil, 0, 0, false
	}
	return data, item, itemStart, itemEnd, true
}

func appendRawCompactionSSEContentPart(item []byte, transcriptText string) ([]byte, bool, bool) {
	var identity struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(item, &identity) != nil || identity.Type != "message" || identity.Role != "assistant" {
		return item, false, false
	}
	contentStart, contentEnd, hasContent := jsonObjectFieldValueRange(item, "content")
	if !hasContent {
		mutated, _, valid := appendRawCompactionAssistantContentPart(item, 0, 0, false, transcriptText)
		return mutated, valid, valid
	}
	contentRanges, valid := jsonArrayValueRanges(item[contentStart:contentEnd])
	if !valid {
		return item, false, false
	}
	for _, contentRange := range contentRanges {
		part := item[contentStart+contentRange.start : contentStart+contentRange.end]
		partType, ok := rawCompactionSSEStringField(part, "type")
		if !ok {
			return item, false, false
		}
		if partType != "output_text" {
			continue
		}
		text, ok := rawCompactionSSEStringField(part, "text")
		if !ok {
			return item, false, false
		}
		if strings.Contains(text, transcriptText) {
			return item, false, true
		}
	}
	mutated, _, valid := appendRawCompactionAssistantContentPart(item, contentStart, contentEnd, true, transcriptText)
	return mutated, valid, valid
}

func rawCompactionSSEContentCount(item []byte) (int, bool) {
	contentStart, contentEnd, hasContent := jsonObjectFieldValueRange(item, "content")
	if !hasContent {
		return 0, true
	}
	ranges, ok := jsonArrayValueRanges(item[contentStart:contentEnd])
	return len(ranges), ok
}

func rawCompactionSSEItemsHaveCoherentContent(first, second []byte) bool {
	firstStart, firstEnd, firstHasContent := jsonObjectFieldValueRange(first, "content")
	secondStart, secondEnd, secondHasContent := jsonObjectFieldValueRange(second, "content")
	if firstHasContent != secondHasContent {
		return false
	}
	if !firstHasContent {
		return true
	}
	var firstParts []rawCompactionSSEContentPart
	var secondParts []rawCompactionSSEContentPart
	if json.Unmarshal(first[firstStart:firstEnd], &firstParts) != nil || json.Unmarshal(second[secondStart:secondEnd], &secondParts) != nil || len(firstParts) != len(secondParts) {
		return false
	}
	for index := range firstParts {
		if firstParts[index] != secondParts[index] {
			return false
		}
	}
	return true
}

func rawCompactionSSESyntheticContentEvents(identity rawCompactionSSEItemIdentity, transcriptText string) ([]byte, bool) {
	emptyAnnotations := make([]json.RawMessage, 0)
	emptyLogprobs := make([]json.RawMessage, 0)
	emptyPart := &rawCompactionSSESyntheticContentPart{Type: "output_text", Text: "", Annotations: emptyAnnotations}
	completePart := &rawCompactionSSESyntheticContentPart{Type: "output_text", Text: transcriptText, Annotations: emptyAnnotations}
	events := []rawCompactionSSESyntheticEvent{
		{Type: string(rawCompactionSSEContentPartAdded), ItemID: identity.id, OutputIndex: identity.outputIndex, ContentIndex: identity.contentIndex, Part: emptyPart, Delta: "", Text: "", Logprobs: nil, SequenceNumber: identity.sequence},
		{Type: string(rawCompactionSSEOutputTextDelta), ItemID: identity.id, OutputIndex: identity.outputIndex, ContentIndex: identity.contentIndex, Part: nil, Delta: transcriptText, Text: "", Logprobs: &emptyLogprobs, SequenceNumber: identity.sequence + 1},
		{Type: string(rawCompactionSSEOutputTextDone), ItemID: identity.id, OutputIndex: identity.outputIndex, ContentIndex: identity.contentIndex, Part: nil, Delta: "", Text: transcriptText, Logprobs: &emptyLogprobs, SequenceNumber: identity.sequence + 2},
		{Type: string(rawCompactionSSEContentPartDone), ItemID: identity.id, OutputIndex: identity.outputIndex, ContentIndex: identity.contentIndex, Part: completePart, Delta: "", Text: "", Logprobs: nil, SequenceNumber: identity.sequence + 3},
	}
	var result []byte
	for _, event := range events {
		var data bytes.Buffer
		encoder := json.NewEncoder(&data)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(event); err != nil {
			return nil, false
		}
		result = append(result, "event: "...)
		result = append(result, event.Type...)
		result = append(result, "\ndata: "...)
		result = append(result, bytes.TrimSuffix(data.Bytes(), []byte("\n"))...)
		result = append(result, '\n', '\n')
	}
	return result, true
}

func replaceRawCompactionSSEItemAndSequence(data, item []byte, sequence int) []byte {
	itemStart, itemEnd, _ := jsonObjectFieldValueRange(data, "item")
	mutated := replaceByteRange(data, itemStart, itemEnd, item)
	mutated, _ = replaceRawCompactionSSEIntegerField(mutated, "sequence_number", sequence)
	return mutated
}

func shiftRawCompactionSSEFrames(frames []byte, previousSequence int) ([]byte, int, bool) {
	if len(frames) == 0 {
		return nil, previousSequence, true
	}
	reader := bufio.NewReader(bytes.NewReader(frames))
	var shifted []byte
	lastSequence := previousSequence
	for {
		frame, readErr, oversized := readRawCompactionSSEFrame(
			reader,
			maxRawCompactionSSEPendingBytes,
		)
		if oversized {
			return nil, 0, false
		}
		if len(frame) == 0 {
			return shifted, lastSequence, errors.Is(readErr, io.EOF)
		}
		_, data, dataCount := rawSSEFrameDataValue(frame)
		if dataCount == 0 && rawSSEFrameIsCommentOnly(frame) {
			shifted = append(shifted, frame...)
			continue
		}
		sequence, ok := rawCompactionSSEIntegerField(data, "sequence_number")
		if dataCount == 0 || !ok || sequence <= lastSequence {
			return nil, 0, false
		}
		mutatedSequence, ok := addRawCompactionSSESyntheticEventCount(sequence)
		if !ok {
			return nil, 0, false
		}
		mutatedData, _ := replaceRawCompactionSSEIntegerField(data, "sequence_number", mutatedSequence)
		shifted = append(shifted, replaceRawSSEFrameData(frame, mutatedData)...)
		lastSequence = sequence
		if readErr != nil {
			return shifted, lastSequence, errors.Is(readErr, io.EOF)
		}
	}
}

func addRawCompactionSSESyntheticEventCount(sequence int) (int, bool) {
	if sequence > math.MaxInt-rawCompactionSSESyntheticEventCount {
		return 0, false
	}
	return sequence + rawCompactionSSESyntheticEventCount, true
}

func rawSSEFrameIsCommentOnly(frame []byte) bool {
	hasComment := false
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimSuffix(line, []byte("\r")))
		if len(line) == 0 {
			continue
		}
		if line[0] != ':' {
			return false
		}
		hasComment = true
	}
	return hasComment
}

func rawCompactionSSEIntegerField(data []byte, field string) (int, bool) {
	start, end, ok := jsonObjectFieldValueRange(data, field)
	if !ok {
		return 0, false
	}
	var value int
	if json.Unmarshal(data[start:end], &value) != nil {
		return 0, false
	}
	return value, true
}

func replaceRawCompactionSSEIntegerField(data []byte, field string, value int) ([]byte, bool) {
	start, end, ok := jsonObjectFieldValueRange(data, field)
	if !ok {
		return data, false
	}
	return replaceByteRange(data, start, end, fmt.Appendf(nil, "%d", value)), true
}

func rawCompactionSSEStringField(data []byte, field string) (string, bool) {
	start, end, ok := jsonObjectFieldValueRange(data, field)
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(data[start:end], &value) != nil {
		return "", false
	}
	return value, true
}

// selectRawCompactionStart renders only a logarithmic number of suffixes when
// enforcing a byte limit. A longer suffix starts at a lower unit index.
func selectRawCompactionStart(
	units []rawCompactionInterval,
	maxBytes int,
	targetCount int,
	render func(start int) (string, bool),
) (int, string, bool) {
	firstCandidate := len(units) - targetCount
	if maxBytes <= 0 {
		rendered, ok := render(units[firstCandidate].start)
		return firstCandidate, rendered, ok && strings.TrimSpace(rendered) != ""
	}

	selected := -1
	selectedTranscript := ""
	lower := firstCandidate
	upper := len(units) - 1
	for lower <= upper {
		middle := lower + (upper-lower)/2
		rendered, ok := render(units[middle].start)
		if !ok || strings.TrimSpace(rendered) == "" {
			return 0, "", false
		}
		if len(rendered) > maxBytes {
			lower = middle + 1
			continue
		}
		selected = middle
		selectedTranscript = rendered
		upper = middle - 1
	}
	if selected < 0 {
		return 0, "", false
	}
	return selected, selectedTranscript, true
}

func appendRawCompactionAssistantContentPart(
	item []byte,
	contentStart int,
	contentEnd int,
	hasContent bool,
	transcriptText string,
) ([]byte, bool, bool) {
	encodedText, ok := marshalRawCompactionString(transcriptText)
	if !ok {
		return item, false, false
	}
	part := append([]byte(`{"type":"output_text","text":`), encodedText...)
	part = append(part, '}')
	if hasContent {
		content := item[contentStart:contentEnd]
		closing := len(content) - 1
		for closing >= 0 && (content[closing] == ' ' || content[closing] == '\t' || content[closing] == '\r' || content[closing] == '\n') {
			closing--
		}
		if closing < 0 || content[closing] != ']' {
			return item, false, false
		}
		hasParts := len(bytes.TrimSpace(content[1:closing])) > 0
		replacement := make([]byte, 0, len(content)+len(part)+1)
		replacement = append(replacement, content[:closing]...)
		if hasParts {
			replacement = append(replacement, ',')
		}
		replacement = append(replacement, part...)
		replacement = append(replacement, content[closing:]...)
		return replaceByteRange(item, contentStart, contentEnd, replacement), true, true
	}
	closing := len(item) - 1
	for closing >= 0 && (item[closing] == ' ' || item[closing] == '\t' || item[closing] == '\r' || item[closing] == '\n') {
		closing--
	}
	if closing < 0 || item[closing] != '}' {
		return item, false, false
	}
	hasFields := len(bytes.TrimSpace(item[1:closing])) > 0
	replacement := make([]byte, 0, len(item)+len(part)+13)
	replacement = append(replacement, item[:closing]...)
	if hasFields {
		replacement = append(replacement, ',')
	}
	replacement = append(replacement, `"content":[`...)
	replacement = append(replacement, part...)
	replacement = append(replacement, ']')
	replacement = append(replacement, item[closing:]...)
	return replacement, true, true
}

func rawSSEFrameEvent(frame []byte) rawCompactionSSEEvent {
	var eventName rawCompactionSSEEvent
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = rawCompactionSSEEvent(strings.TrimSpace(string(line[len("event:"):])))
		}
	}
	return eventName
}

func rawSSEFrameDataValue(frame []byte) (string, []byte, int) {
	eventName := ""
	data := make([]byte, 0, len(frame))
	dataCount := 0
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		field, value := rawSSEField(line)
		if bytes.Equal(field, []byte("event")) {
			eventName = string(value)
		}
		if !bytes.Equal(field, []byte("data")) {
			continue
		}
		if dataCount > 0 {
			data = append(data, '\n')
		}
		data = append(data, value...)
		dataCount++
	}
	return eventName, data, dataCount
}

func rawSSEField(line []byte) ([]byte, []byte) {
	if len(line) == 0 || line[0] == ':' {
		return nil, nil
	}
	field, value, found := bytes.Cut(line, []byte(":"))
	if !found {
		return line, nil
	}
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}

func replaceRawSSEFrameData(frame []byte, data []byte) []byte {
	var compacted bytes.Buffer
	if json.Compact(&compacted, data) == nil {
		data = compacted.Bytes()
	}
	firstDataLineStart := -1
	var result bytes.Buffer
	lineStart := 0
	for lineStart < len(frame) {
		lineEnd := bytes.IndexByte(frame[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(frame)
		} else {
			lineEnd += lineStart + 1
		}
		line := frame[lineStart:lineEnd]
		lineContent := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		field, _ := rawSSEField(lineContent)
		if !bytes.Equal(field, []byte("data")) {
			result.Write(line)
			lineStart = lineEnd
			continue
		}
		if firstDataLineStart < 0 {
			firstDataLineStart = lineStart
			result.WriteString("data: ")
			result.Write(data)
			if bytes.HasSuffix(line, []byte("\r\n")) {
				result.WriteString("\r\n")
			} else if bytes.HasSuffix(line, []byte("\n")) {
				result.WriteByte('\n')
			}
		}
		lineStart = lineEnd
	}
	return result.Bytes()
}
