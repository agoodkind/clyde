package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
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
	inner      io.ReadCloser
	reader     *bufio.Reader
	transcript string
	pending    []byte
	pendingErr error
	candidate  []byte
	disabled   bool
}

func newRawCompactionSSEBody(inner io.ReadCloser, transcriptText string) *rawCompactionSSEBody {
	return &rawCompactionSSEBody{
		inner:      inner,
		reader:     bufio.NewReader(inner),
		transcript: transcriptText,
		pending:    nil,
		pendingErr: nil,
		candidate:  nil,
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
		if readErr != nil && len(b.candidate) > 0 {
			return b.flushCandidateBeforeEOFFrame(frame, readErr)
		}
		return b.queueSSEBytes(frame, readErr)
	}
}

func (b *rawCompactionSSEBody) handleSSECandidateFrame(frame []byte, readErr error) error {
	mutated, matched, valid := appendRawCompactionSSEFrame(frame, b.transcript)
	if !valid {
		return b.failOpenSSE(frame, readErr)
	}
	if !matched {
		if readErr != nil && len(b.candidate) > 0 {
			return b.flushCandidateBeforeEOFFrame(frame, readErr)
		}
		return b.queueSSEBytes(frame, readErr)
	}
	if readErr != nil {
		b.pending = joinRawCompactionSSEFrames(b.candidate, mutated)
		b.candidate = nil
		return b.queueSSEError(readErr)
	}
	if len(b.candidate) > 0 {
		b.pending = b.candidate
	}
	b.candidate = frame
	return nil
}

func (b *rawCompactionSSEBody) handleSSECompletedFrame(frame []byte, readErr error) error {
	if !rawCompactionSSEJSONFrameIsValid(frame, rawCompactionSSECompleted) {
		return b.failOpenSSE(frame, readErr)
	}
	mutated, ok := b.mutatedSSECandidate()
	if !ok {
		return b.failOpenSSE(frame, readErr)
	}
	b.pending = joinRawCompactionSSEFrames(mutated, frame)
	b.candidate = nil
	return b.queueSSEError(readErr)
}

func (b *rawCompactionSSEBody) flushCandidateAtEOF(readErr error) error {
	if len(b.candidate) == 0 {
		return readErr
	}
	mutated, ok := b.mutatedSSECandidate()
	if !ok {
		mutated = b.candidate
		b.disabled = true
	}
	b.pending = mutated
	b.candidate = nil
	return b.queueSSEError(readErr)
}

func (b *rawCompactionSSEBody) flushCandidateBeforeEOFFrame(frame []byte, readErr error) error {
	mutated, ok := b.mutatedSSECandidate()
	if !ok {
		return b.failOpenSSE(frame, readErr)
	}
	b.pending = joinRawCompactionSSEFrames(mutated, frame)
	b.candidate = nil
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
	b.pending = joinRawCompactionSSEFrames(b.candidate, frame)
	b.candidate = nil
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
	gotEvent, dataStart, dataEnd, dataCount := rawSSEFrameData(frame)
	if gotEvent != string(eventName) || dataCount != 1 || !json.Valid(frame[dataStart:dataEnd]) {
		return false
	}
	var payload struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(frame[dataStart:dataEnd], &payload) == nil && payload.Type == string(eventName)
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
	eventName, dataStart, dataEnd, dataCount := rawSSEFrameData(frame)
	if eventName != string(rawCompactionSSEOutputItemDone) {
		return frame, false, true
	}
	if dataCount != 1 {
		return frame, false, false
	}
	itemStart, itemEnd, ok := jsonObjectFieldValueRange(frame[dataStart:dataEnd], "item")
	if !ok {
		return frame, false, false
	}
	itemStart += dataStart
	itemEnd += dataStart
	mutated, matched, valid := appendRawCompactionAssistantItem(frame[itemStart:itemEnd], transcriptText)
	if !valid || !matched {
		return frame, matched, valid
	}
	if bytes.Equal(mutated, frame[itemStart:itemEnd]) {
		return frame, true, true
	}
	return replaceByteRange(frame, itemStart, itemEnd, mutated), true, true
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

func rawSSEFrameData(frame []byte) (string, int, int, int) {
	eventName := ""
	dataStart := 0
	dataEnd := 0
	dataCount := 0
	lineStart := 0
	for lineStart < len(frame) {
		lineEnd := bytes.IndexByte(frame[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(frame)
		} else {
			lineEnd += lineStart
		}
		contentEnd := lineEnd
		if contentEnd > lineStart && frame[contentEnd-1] == '\r' {
			contentEnd--
		}
		line := frame[lineStart:contentEnd]
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = strings.TrimSpace(string(line[len("event:"):]))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			dataCount++
			valueStart := lineStart + len("data:")
			if valueStart < contentEnd && frame[valueStart] == ' ' {
				valueStart++
			}
			dataStart = valueStart
			dataEnd = contentEnd
		}
		if lineEnd >= len(frame) {
			break
		}
		lineStart = lineEnd + 1
	}
	return eventName, dataStart, dataEnd, dataCount
}
