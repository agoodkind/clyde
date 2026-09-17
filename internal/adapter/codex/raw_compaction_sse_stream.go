package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
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
	inner             io.ReadCloser
	reader            *bufio.Reader
	transcript        string
	pending           []byte
	pendingErr        error
	candidate         []byte
	following         []byte
	disabled          bool
	onMutated         func()
	strictFinalAnswer bool
}

type rawCompactionMutation struct {
	mutated atomic.Bool
}

// NewRawResponsesCompactionV2FinalAnswerTransformer creates the one-shot
// recovery transformer only for a regular final-answer request.
func NewRawResponsesCompactionV2FinalAnswerTransformer(request RawResponsesRequest, recovery *RawResponsesCompactionV2Recovery) *RawResponsesCompactionTransformer {
	if recovery == nil || !rawResponsesCompactionV2FinalAnswerTurn(request.Header) {
		return nil
	}
	return &RawResponsesCompactionTransformer{transcript: recovery.transcript, stream: request.Stream, mutation: &rawCompactionMutation{mutated: atomic.Bool{}}, strictFinalAnswer: rawResponsesCompactionV2FinalAnswerTurn(request.Header)}
}

// DidMutateResponse reports whether this transformer produced tagged output.
func (t *RawResponsesCompactionTransformer) DidMutateResponse() bool {
	return t != nil && t.mutation != nil && t.mutation.mutated.Load()
}

func (t *RawResponsesCompactionTransformer) markMutated() {
	if t != nil && t.mutation != nil {
		t.mutation.mutated.Store(true)
	}
}

func rawResponsesCompactionV2FinalAnswerTurn(header http.Header) bool {
	var metadata rawResponsesCompactionMetadata
	if json.Unmarshal([]byte(header.Get(CodexTurnMetadataHeader)), &metadata) != nil {
		return false
	}
	return metadata.RequestKind == "turn" && metadata.Compaction.Phase == "final_answer"
}

func newRawCompactionSSEBody(inner io.ReadCloser, transcriptText string, onMutatedCallbacks ...func()) *rawCompactionSSEBody {
	var onMutated func()
	if len(onMutatedCallbacks) > 0 {
		onMutated = onMutatedCallbacks[0]
	}
	return &rawCompactionSSEBody{
		inner:             inner,
		reader:            bufio.NewReader(inner),
		transcript:        transcriptText,
		pending:           nil,
		pendingErr:        nil,
		candidate:         nil,
		following:         nil,
		disabled:          false,
		onMutated:         onMutated,
		strictFinalAnswer: false,
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
		_, _, dataCount := rawSSEFrameDataValue(frame)
		if readErr == nil && dataCount == 0 && rawSSEFrameIsCommentOnly(frame) {
			return b.queueSSEBytes(frame, readErr)
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
	if b.strictFinalAnswer && !rawCompactionStrictFinalAnswerSSEFrame(frame) {
		return b.failOpenSSE(frame, readErr)
	}
	mutatedFrames, ok := appendRawCompactionSSEStreamEvents(b.candidate, b.following, frame, b.transcript)
	if !ok {
		return b.failOpenSSE(frame, readErr)
	}
	originalFrames := joinRawCompactionSSEFrames(joinRawCompactionSSEFrames(b.candidate, b.following), frame)
	mutated := !bytes.Equal(mutatedFrames, originalFrames)
	b.pending = mutatedFrames
	b.candidate = nil
	b.following = nil
	if b.onMutated != nil && mutated {
		b.onMutated()
	}
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
	_, data, dataCount := rawSSEFrameDataValue(frame)
	if dataCount == 0 {
		return true
	}
	return json.Valid(data)
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
		value, err := reader.ReadByte()
		if err != nil {
			return frame.Bytes(), err, false
		}
		frame.WriteByte(value)
		if frame.Len() > maxBytes {
			return frame.Bytes(), nil, true
		}
		if value != '\n' && value != '\r' {
			continue
		}
		oversized, readErr := consumeRawCompactionSSECarriageReturn(reader, &frame, value, maxBytes)
		if readErr != nil {
			return frame.Bytes(), readErr, false
		}
		if oversized {
			return frame.Bytes(), nil, true
		}
		if rawCompactionSSEBlankLine(frame.Bytes()[lineStart:]) {
			return frame.Bytes(), nil, false
		}
		lineStart = frame.Len()
	}
}

func consumeRawCompactionSSECarriageReturn(reader *bufio.Reader, frame *bytes.Buffer, value byte, maxBytes int) (bool, error) {
	if value != '\r' {
		return false, nil
	}
	if reader.Buffered() == 0 {
		return false, nil
	}
	next, _ := reader.Peek(1)
	if len(next) != 1 || next[0] != '\n' {
		return false, nil
	}
	lineFeed, readErr := reader.ReadByte()
	if readErr != nil {
		slog.Warn("adapter.codex.raw_compaction.sse_read_crlf_failed", "concern", "adapter.providers.codex.request", "err", readErr)
		return false, fmt.Errorf("read CRLF terminator: %w", readErr)
	}
	frame.WriteByte(lineFeed)
	return frame.Len() > maxBytes, nil
}

func rawCompactionSSEBlankLine(line []byte) bool {
	return bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\r"))
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
