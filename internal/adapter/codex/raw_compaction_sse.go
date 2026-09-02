package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
)

type rawCompactionSSEEvent string

const (
	rawCompactionSSEOutputItemDone rawCompactionSSEEvent = "response.output_item.done"
	rawCompactionSSECompleted      rawCompactionSSEEvent = "response.completed"
	rawCompactionSSEFailed         rawCompactionSSEEvent = "response.failed"
	rawCompactionSSEIncomplete     rawCompactionSSEEvent = "response.incomplete"
	rawCompactionSSEError          rawCompactionSSEEvent = "error"
)

type rawCompactionSSEBody struct {
	inner                  io.ReadCloser
	reader                 *bufio.Reader
	transcript             string
	pending                []byte
	pendingErr             error
	candidate              []byte
	following              []byte
	disabled               bool
	onMutated              func()
	requireCompletedStatus bool
}

type rawCompactionMutation struct {
	mutated atomic.Bool
}

// NewRawResponsesCompactionV2FinalAnswerTransformer creates the one-shot
// recovery transformer only for a regular final-answer request.
func NewRawResponsesCompactionV2FinalAnswerTransformer(request RawResponsesRequest, recovery *RawResponsesCompactionV2Recovery) *RawResponsesCompactionTransformer {
	if recovery == nil || !rawResponsesCompactionV2FinalAnswer(request.Header) {
		return nil
	}
	return &RawResponsesCompactionTransformer{transcript: recovery.transcript, stream: request.Stream, mutation: &rawCompactionMutation{mutated: atomic.Bool{}}}
}

// DidMutateResponse reports whether this transformer produced tagged output.
func (t *RawResponsesCompactionTransformer) DidMutateResponse() bool {
	return t != nil && t.mutation != nil && t.mutation.mutated.Load()
}

// RequiresTerminalValidation reports whether recovery completion depends on a
// successful response mutation.
func (t *RawResponsesCompactionTransformer) RequiresTerminalValidation() bool {
	return t != nil && t.mutation != nil
}

func (t *RawResponsesCompactionTransformer) markMutated() {
	if t != nil && t.mutation != nil {
		t.mutation.mutated.Store(true)
	}
}

func rawResponsesCompactionV2FinalAnswer(header http.Header) bool {
	var metadata rawResponsesCompactionMetadata
	if json.Unmarshal([]byte(header.Get(CodexTurnMetadataHeader)), &metadata) != nil {
		return false
	}
	return metadata.RequestKind == "turn" && metadata.Compaction.Implementation == "" && metadata.Compaction.Strategy == "" && metadata.Compaction.Phase == "final_answer"
}

func rawResponsesCompactionV2RegularFinalAnswer(header http.Header) bool {
	return rawResponsesCompactionV2FinalAnswer(header)
}

func rawResponsesCompactionV2FinalAnswerTurn(header http.Header) bool {
	var metadata rawResponsesCompactionMetadata
	if json.Unmarshal([]byte(header.Get(CodexTurnMetadataHeader)), &metadata) != nil {
		return false
	}
	return rawResponsesCompactionV2RegularTurn(header) && metadata.Compaction.Phase == "final_answer"
}

func newRawCompactionSSEBody(inner io.ReadCloser, transcriptText string, onMutated func(), requireCompletedStatus bool) *rawCompactionSSEBody {
	return &rawCompactionSSEBody{
		inner:                  inner,
		reader:                 bufio.NewReader(inner),
		transcript:             transcriptText,
		pending:                nil,
		pendingErr:             nil,
		candidate:              nil,
		following:              nil,
		disabled:               false,
		onMutated:              onMutated,
		requireCompletedStatus: requireCompletedStatus,
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
	frame, readErr := readRawCompactionSSEFrame(b.reader)
	if len(frame) == 0 {
		return b.flushCandidateAtEOF(readErr)
	}
	if b.disabled {
		return b.queueSSEBytes(frame, readErr)
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
	default:
		if len(b.candidate) > 0 {
			if readErr != nil || !rawCompactionUnknownSSEFrameIsValid(frame) {
				return b.failOpenSSE(frame, readErr)
			}
			_, _, dataCount := rawSSEFrameDataValue(frame)
			if dataCount == 0 {
				return b.queueSSEBytes(frame, readErr)
			}
			b.following = append(b.following, frame...)
			return nil
		}
		return b.queueSSEBytes(frame, readErr)
	}
}

func (b *rawCompactionSSEBody) handleSSECandidateFrame(frame []byte, readErr error) error {
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
	if len(b.candidate) > 0 {
		b.pending = joinRawCompactionSSEFrames(b.candidate, b.following)
	}
	b.candidate = frame
	b.following = nil
	return nil
}

func (b *rawCompactionSSEBody) handleSSECompletedFrame(frame []byte, readErr error) error {
	if readErr != nil {
		return b.failOpenSSE(frame, readErr)
	}
	if !rawCompactionSSEJSONFrameIsValid(frame, rawCompactionSSECompleted) || (b.requireCompletedStatus && !rawCompactionSSECompletedFrameIsSuccessful(frame)) {
		return b.failOpenSSE(frame, readErr)
	}
	if len(b.candidate) == 0 {
		b.pending = frame
		return b.queueSSEError(readErr)
	}
	mutatedCandidate, candidateOK := b.mutatedSSECandidate()
	if !candidateOK {
		return b.failOpenSSE(frame, readErr)
	}
	mutatedCompleted, completedOK := appendRawCompactionSSECompletedFrame(frame, b.transcript)
	if !completedOK {
		return b.failOpenSSE(frame, readErr)
	}
	b.pending = joinRawCompactionSSEFrames(
		joinRawCompactionSSEFrames(mutatedCandidate, b.following),
		mutatedCompleted,
	)
	b.candidate = nil
	b.following = nil
	if b.onMutated != nil && !bytes.Equal(mutatedCandidate, b.candidate) {
		b.onMutated()
	}
	return b.queueSSEError(readErr)
}

func rawCompactionSSECompletedFrameIsSuccessful(frame []byte) bool {
	if !rawCompactionSSEJSONFrameIsValid(frame, rawCompactionSSECompleted) {
		return false
	}
	_, data, _ := rawSSEFrameDataValue(frame)
	var payload struct {
		Response struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	return json.Unmarshal(data, &payload) == nil && payload.Response.Status == "completed"
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

func (b *rawCompactionSSEBody) mutatedSSECandidate() ([]byte, bool) {
	if len(b.candidate) == 0 {
		return nil, true
	}
	mutated, matched, valid := appendRawCompactionSSEFrame(b.candidate, b.transcript)
	return mutated, matched && valid
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
	_, data, dataCount := rawSSEFrameDataValue(frame)
	return dataCount == 0 || json.Valid(data)
}

func (b *rawCompactionSSEBody) Close() error {
	if err := b.inner.Close(); err != nil {
		slog.Warn("adapter.codex.raw_compaction.sse_close_failed", "concern", "adapter.providers.codex.request", "err", err)
		return fmt.Errorf("close raw compaction SSE response: %w", err)
	}
	return nil
}

func readRawCompactionSSEFrame(reader *bufio.Reader) ([]byte, error) {
	var frame bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		frame.Write(line)
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			return frame.Bytes(), err
		}
		if err != nil {
			return frame.Bytes(), err
		}
	}
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

func appendRawCompactionSSECompletedFrame(frame []byte, transcriptText string) ([]byte, bool) {
	eventName, data, dataCount := rawSSEFrameDataValue(frame)
	if eventName != string(rawCompactionSSECompleted) || dataCount == 0 {
		return frame, false
	}
	responseStart, responseEnd, hasResponse := jsonObjectFieldValueRange(data, "response")
	if !hasResponse {
		return frame, true
	}
	response := data[responseStart:responseEnd]
	if _, _, hasOutput := jsonObjectFieldValueRange(response, "output"); !hasOutput {
		return frame, true
	}
	mutatedResponse, ok := appendRawCompactionJSON(response, transcriptText)
	if !ok {
		return frame, false
	}
	if bytes.Equal(mutatedResponse, response) {
		return frame, true
	}
	mutatedData := replaceByteRange(data, responseStart, responseEnd, mutatedResponse)
	return replaceRawSSEFrameData(frame, mutatedData), true
}

func rawSSEFrameEvent(frame []byte) rawCompactionSSEEvent {
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("event:")) {
			return rawCompactionSSEEvent(strings.TrimSpace(string(line[len("event:"):])))
		}
	}
	return ""
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
