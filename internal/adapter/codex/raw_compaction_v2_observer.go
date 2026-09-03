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
}

func (b *rawResponsesCompactionV2ObservedBody) Read(destination []byte) (int, error) {
	n, err := b.source.Read(destination)
	if n > 0 {
		_, _ = b.captured.Write(destination[:n])
		b.armBeforeTerminalFrame(destination[:n])
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("read observed compaction response: %w", err)
	}
	if err == nil {
		return n, nil
	}
	return n, io.EOF
}

func (b *rawResponsesCompactionV2ObservedBody) armBeforeTerminalFrame(chunk []byte) {
	if b.armed || !strings.Contains(strings.ToLower(b.contentType), "text/event-stream") ||
		strings.TrimSpace(b.contentEncoding) != "" || b.captured.Truncated() || b.sseInvalid {
		return
	}
	if len(b.sseBuffer)+len(chunk) > maxRawResponsesCompactionV2ObserveBytes {
		b.sseInvalid = true
		return
	}
	b.sseBuffer = append(b.sseBuffer, chunk...)
	for {
		frame, remainder, complete := rawResponsesCompactionV2SSEFrame(b.sseBuffer)
		if !complete {
			return
		}
		b.sseBuffer = remainder
		if !rawResponsesCompactionV2SSEDataIsValid(rawResponsesCompactionV2SSEFrameData(frame), &b.sseEncrypted, &b.sseCompleted) {
			b.sseInvalid = true
			return
		}
	}
}

func rawResponsesCompactionV2SSEFrame(body []byte) ([]byte, []byte, bool) {
	lfIndex := bytes.Index(body, []byte("\n\n"))
	crlfIndex := bytes.Index(body, []byte("\r\n\r\n"))
	if lfIndex < 0 && crlfIndex < 0 {
		return nil, body, false
	}
	if crlfIndex >= 0 && (lfIndex < 0 || crlfIndex < lfIndex) {
		return body[:crlfIndex], body[crlfIndex+4:], true
	}
	return body[:lfIndex], body[lfIndex+2:], true
}

func rawResponsesCompactionV2SSEFrameData(frame []byte) []string {
	data := make([]string, 0, 1)
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, string(value))
		}
	}
	return data
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
	encrypted, ok := rawResponsesCompactionV2EncryptedContent(body.captured.Bytes(), body.contentType)
	if !ok {
		return
	}
	body.registry.Disarm(body.plan.SessionID, encrypted, body.armGeneration)
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
	if !b.registry.Arm(b.plan.SessionID, encrypted, b.plan.Transcript) {
		return
	}
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
	slog.Debug("adapter.codex.raw_compaction_v2_observer",
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
	scanner := bufio.NewScanner(bytes.NewReader(body))
	dataFrameCount := 0
	compactionItemCount := 0
	completedCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		dataFrameCount++
		var value struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &value) != nil {
			continue
		}
		if value.Type == "response.output_item.done" && value.Item.Type == "compaction" {
			compactionItemCount++
		}
		if value.Type == "response.completed" {
			completedCount++
		}
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
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), maxRawResponsesCompactionV2ObserveBytes+1)
	completed := false
	encrypted := ""
	data := make([]string, 0, 1)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !rawResponsesCompactionV2SSEDataIsValid(data, &encrypted, &completed) {
				return "", false
			}
			data = data[:0]
			continue
		}
		if dataLine, ok := strings.CutPrefix(line, "data:"); ok {
			data = append(data, dataLine)
		}
	}
	if scanner.Err() != nil {
		return "", false
	}
	if !rawResponsesCompactionV2SSEDataIsValid(data, &encrypted, &completed) {
		return "", false
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
