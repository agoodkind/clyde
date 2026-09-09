package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	"goodkind.io/clyde/internal/mitm/capture"
)

const maxRawResponsesCompactionV2ObserveBytes = 8 * 1024 * 1024

// ObserveRawResponsesCompactionV2Response captures a successful response for later arming.
func ObserveRawResponsesCompactionV2Response(response *http.Response, plan RawResponsesCompactionV2Plan, registry *RawResponsesCompactionV2Registry) *http.Response {
	if response == nil || response.StatusCode < 200 || response.StatusCode >= 300 || registry == nil {
		return response
	}
	clone := *response
	clone.Body = &rawResponsesCompactionV2ObservedBody{
		source:          response.Body,
		captured:        capture.NewCappedBuffer(maxRawResponsesCompactionV2ObserveBytes),
		plan:            plan,
		registry:        registry,
		contentType:     response.Header.Get("Content-Type"),
		contentEncoding: response.Header.Get("Content-Encoding"),
		armed:           false,
		armGeneration:   0,
		sseBuffer:       nil,
		sseScanOffset:   0,
		sseEncrypted:    "",
		sseCompleted:    false,
		sseInvalid:      false,
	}
	return &clone
}

type rawResponsesCompactionV2ObservedBody struct {
	source          io.ReadCloser
	captured        *capture.CappedBuffer
	plan            RawResponsesCompactionV2Plan
	registry        *RawResponsesCompactionV2Registry
	contentType     string
	contentEncoding string
	armed           bool
	armGeneration   uint64
	sseBuffer       []byte
	sseScanOffset   int
	sseEncrypted    string
	sseCompleted    bool
	sseInvalid      bool
}

func (b *rawResponsesCompactionV2ObservedBody) Read(destination []byte) (int, error) {
	n, err := b.source.Read(destination)
	if n > 0 {
		_, _ = b.captured.Write(destination[:n])
		b.armBeforeTerminalFrame(destination[:n])
	}
	if err == io.EOF {
		b.validateSSETailAtEOF()
	}
	if err != nil && err != io.EOF {
		b.invalidateSSE()
		return n, fmt.Errorf("read observed compaction response: %w", err)
	}
	if err == nil {
		return n, nil
	}
	return n, io.EOF
}

func (b *rawResponsesCompactionV2ObservedBody) validateSSETailAtEOF() {
	if b.captured.Truncated() {
		b.invalidateSSE()
		return
	}
	if b.sseInvalid {
		return
	}
	b.consumeSSEFrames(true)
	if len(b.sseBuffer) > 0 {
		b.invalidateSSE()
	}
}

func (b *rawResponsesCompactionV2ObservedBody) armBeforeTerminalFrame(chunk []byte) {
	if !strings.Contains(strings.ToLower(b.contentType), "text/event-stream") ||
		strings.TrimSpace(b.contentEncoding) != "" || b.sseInvalid {
		return
	}
	if b.captured.Truncated() {
		b.invalidateSSE()
		return
	}
	if len(b.sseBuffer)+len(chunk) > maxRawResponsesCompactionV2ObserveBytes {
		b.invalidateSSE()
		return
	}
	b.sseBuffer = append(b.sseBuffer, chunk...)
	b.consumeSSEFrames(false)
}

func (b *rawResponsesCompactionV2ObservedBody) consumeSSEFrames(atEOF bool) {
	for {
		frame, remainder, complete, nextScanOffset := rawResponsesCompactionV2SSEFrameFrom(
			b.sseBuffer,
			b.sseScanOffset,
			atEOF,
		)
		if !complete {
			b.sseScanOffset = nextScanOffset
			break
		}
		b.sseBuffer = remainder
		b.sseScanOffset = 0
		if b.sseCompleted {
			b.invalidateSSE()
			return
		}
		if !rawResponsesCompactionV2SSEDataIsValid(rawResponsesCompactionV2SSEFrameData(frame), &b.sseEncrypted, &b.sseCompleted) {
			b.invalidateSSE()
			return
		}
	}
	if b.sseCompleted && len(b.sseBuffer) == 0 {
		b.armEncrypted(b.sseEncrypted)
	}
}

func (b *rawResponsesCompactionV2ObservedBody) invalidateSSE() {
	if b.armed {
		b.registry.Disarm(b.plan.SessionID, b.sseEncrypted, b.armGeneration)
		b.armed = false
	}
	b.sseInvalid = true
	b.sseBuffer = nil
	b.sseScanOffset = 0
}

func rawResponsesCompactionV2SSEFrame(body []byte) ([]byte, []byte, bool) {
	frame, remainder, complete, _ := rawResponsesCompactionV2SSEFrameFrom(body, 0, true)
	return frame, remainder, complete
}

func rawResponsesCompactionV2SSEFrameFrom(body []byte, scanOffset int, atEOF bool) ([]byte, []byte, bool, int) {
	if scanOffset < 0 || scanOffset > len(body) {
		scanOffset = 0
	}
	for index := scanOffset; index < len(body); {
		firstSize := rawResponsesCompactionV2SSEScannableLineEndingSize(body, index, atEOF)
		if firstSize == 0 {
			index++
			continue
		}
		secondIndex := index + firstSize
		secondSize := rawResponsesCompactionV2SSEScannableLineEndingSize(body, secondIndex, atEOF)
		if secondSize > 0 {
			return body[:index], body[secondIndex+secondSize:], true, 0
		}
		index = secondIndex
	}
	return nil, body, false, max(0, len(body)-3)
}

func rawResponsesCompactionV2SSEScannableLineEndingSize(body []byte, index int, atEOF bool) int {
	if !atEOF && index == len(body)-1 && body[index] == '\r' {
		return 0
	}
	return rawResponsesCompactionV2SSELineEndingSize(body, index)
}

func rawResponsesCompactionV2SSELineEndingSize(body []byte, index int) int {
	if index >= len(body) {
		return 0
	}
	if body[index] == '\n' {
		return 1
	}
	if body[index] != '\r' {
		return 0
	}
	if index+1 < len(body) && body[index+1] == '\n' {
		return 2
	}
	return 1
}

func rawResponsesCompactionV2SSEFrameData(frame []byte) []string {
	data := make([]string, 0, 1)
	for _, line := range rawResponsesCompactionV2SSELines(frame) {
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, string(value))
		}
	}
	return data
}

func rawResponsesCompactionV2SSELines(frame []byte) [][]byte {
	lines := make([][]byte, 0, 1)
	lineStart := 0
	for index := 0; index < len(frame); index++ {
		endingSize := rawResponsesCompactionV2SSELineEndingSize(frame, index)
		if endingSize == 0 {
			continue
		}
		lines = append(lines, frame[lineStart:index])
		index += endingSize - 1
		lineStart = index + 1
	}
	if lineStart < len(frame) {
		lines = append(lines, frame[lineStart:])
	}
	return lines
}

// ArmRawResponsesCompactionV2Response arms recovery after a client copy succeeds.
func ArmRawResponsesCompactionV2Response(response *http.Response) {
	if response == nil {
		return
	}
	body, ok := response.Body.(*rawResponsesCompactionV2ObservedBody)
	if !ok {
		return
	}
	body.arm(true)
}

// ReleaseRawResponsesCompactionV2Response removes recovery armed before a
// terminal SSE frame when the client write fails.
func ReleaseRawResponsesCompactionV2Response(response *http.Response) {
	if response == nil {
		return
	}
	body, ok := response.Body.(*rawResponsesCompactionV2ObservedBody)
	if !ok || !body.armed {
		return
	}
	body.registry.Disarm(body.plan.SessionID, body.sseEncrypted, body.armGeneration)
	body.armed = false
}

func (b *rawResponsesCompactionV2ObservedBody) Close() error {
	if err := b.source.Close(); err != nil {
		slog.Warn("adapter.codex.raw_compaction_v2_observer_close_failed", "concern", "adapter.providers.codex.request", "err", err)
		return fmt.Errorf("close observed compaction response: %w", err)
	}
	return nil
}

func (b *rawResponsesCompactionV2ObservedBody) arm(callbackInvoked bool) {
	body := b.captured.Bytes()
	diagnostics := rawResponsesCompactionV2ObservationDiagnostics{
		contentType:         b.contentType,
		contentEncoding:     b.contentEncoding,
		captureTruncated:    b.captured.Truncated(),
		armCallbackInvoked:  callbackInvoked,
		SSEDataFrameCount:   0,
		compactionItemCount: 0,
		completedCount:      0,
		encryptedExtracted:  false,
	}
	if strings.Contains(strings.ToLower(b.contentType), "text/event-stream") {
		diagnostics.SSEDataFrameCount, diagnostics.compactionItemCount, diagnostics.completedCount = rawResponsesCompactionV2SSECounts(body)
	}
	if b.armed || diagnostics.captureTruncated {
		b.logArmDiagnostics(diagnostics)
		return
	}
	if strings.EqualFold(strings.TrimSpace(b.contentEncoding), "zstd") || strings.EqualFold(strings.TrimSpace(b.contentEncoding), "zstandard") {
		decoder, err := zstd.NewReader(
			bytes.NewReader(body),
			zstd.WithDecoderMaxMemory(maxRawResponsesCompactionV2ObserveBytes),
		)
		if err != nil {
			b.logArmDiagnostics(diagnostics)
			return
		}
		defer decoder.Close()
		decoded, err := io.ReadAll(io.LimitReader(decoder, maxRawResponsesCompactionV2ObserveBytes+1))
		if err != nil || len(decoded) > maxRawResponsesCompactionV2ObserveBytes {
			b.logArmDiagnostics(diagnostics)
			return
		}
		body = decoded
		if strings.Contains(strings.ToLower(b.contentType), "text/event-stream") {
			diagnostics.SSEDataFrameCount, diagnostics.compactionItemCount, diagnostics.completedCount = rawResponsesCompactionV2SSECounts(body)
		}
	}
	encrypted, ok := rawResponsesCompactionV2EncryptedContent(body, b.contentType)
	diagnostics.encryptedExtracted = ok
	b.logArmDiagnostics(diagnostics)
	if ok {
		b.armEncrypted(encrypted)
	}
}

func (b *rawResponsesCompactionV2ObservedBody) armEncrypted(encrypted string) {
	if b.armed {
		return
	}
	generation, armed := b.registry.ArmWithGeneration(b.plan.SessionID, encrypted, b.plan.Transcript)
	if !armed {
		return
	}
	b.armGeneration = generation
	b.armed = true
}

type rawResponsesCompactionV2ObservationDiagnostics struct {
	contentType         string
	contentEncoding     string
	captureTruncated    bool
	armCallbackInvoked  bool
	SSEDataFrameCount   int
	compactionItemCount int
	completedCount      int
	encryptedExtracted  bool
}

func (b *rawResponsesCompactionV2ObservedBody) logArmDiagnostics(diagnostics rawResponsesCompactionV2ObservationDiagnostics) {
	slog.Debug(
		"adapter.codex.raw_compaction_v2_observer",
		"response_content_type", diagnostics.contentType,
		"response_content_encoding", diagnostics.contentEncoding,
		"sse_data_frame_count", diagnostics.SSEDataFrameCount,
		"compaction_item_count", diagnostics.compactionItemCount,
		"completed_count", diagnostics.completedCount,
		"capture_truncated", diagnostics.captureTruncated,
		"encrypted_extraction_result", diagnostics.encryptedExtracted,
		"arm_callback_invocation", diagnostics.armCallbackInvoked,
	)
}

func rawResponsesCompactionV2SSECounts(body []byte) (int, int, int) {
	dataFrameCount := 0
	compactionItemCount := 0
	completedCount := 0
	remaining := body
	for len(remaining) > 0 {
		frame, next, complete := rawResponsesCompactionV2SSEFrame(remaining)
		if !complete {
			frame = remaining
			next = nil
		}
		data := rawResponsesCompactionV2SSEFrameData(frame)
		dataFrameCount += len(data)
		var value struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(strings.Join(data, "\n")), &value) == nil {
			if value.Type == "response.output_item.done" && value.Item.Type == "compaction" {
				compactionItemCount++
			}
			if value.Type == "response.completed" {
				completedCount++
			}
		}
		remaining = next
	}
	return dataFrameCount, compactionItemCount, completedCount
}

func rawResponsesCompactionV2EncryptedContent(body []byte, contentType string) (string, bool) {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return rawResponsesCompactionV2SSEEncryptedContent(body)
	}
	var response struct {
		Output []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"output"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "", false
	}
	return rawResponsesCompactionV2OneEncryptedContent(response.Output)
}

func rawResponsesCompactionV2SSEEncryptedContent(body []byte) (string, bool) {
	completed := false
	encrypted := ""
	remaining := body
	for len(remaining) > 0 {
		frame, next, complete := rawResponsesCompactionV2SSEFrame(remaining)
		if !complete {
			return "", false
		}
		if completed || !rawResponsesCompactionV2SSEDataIsValid(rawResponsesCompactionV2SSEFrameData(frame), &encrypted, &completed) {
			return "", false
		}
		remaining = next
	}
	return encrypted, encrypted != "" && completed
}

func rawResponsesCompactionV2SSEDataIsValid(data []string, encrypted *string, completed *bool) bool {
	if len(data) == 0 {
		return true
	}
	var value struct {
		Type string `json:"type"`
		Item struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"item"`
	}
	if json.Unmarshal([]byte(strings.Join(data, "\n")), &value) != nil {
		return false
	}
	if value.Type == "response.output_item.done" {
		if value.Item.Type != "compaction" {
			return true
		}
		if *completed || *encrypted != "" || strings.TrimSpace(value.Item.EncryptedContent) == "" {
			return false
		}
		*encrypted = value.Item.EncryptedContent
	}
	if value.Type == "response.completed" {
		if *encrypted == "" || *completed {
			return false
		}
		*completed = true
	}
	return true
}

func rawResponsesCompactionV2OneEncryptedContent(items []struct {
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
},
) (string, bool) {
	encrypted := ""
	for _, item := range items {
		if item.Type != "compaction" {
			continue
		}
		if encrypted != "" || strings.TrimSpace(item.EncryptedContent) == "" {
			return "", false
		}
		encrypted = item.EncryptedContent
	}
	return encrypted, encrypted != ""
}
