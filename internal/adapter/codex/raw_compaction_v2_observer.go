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
	clone.Body = &rawResponsesCompactionV2ObservedBody{source: response.Body, captured: capture.NewCappedBuffer(maxRawResponsesCompactionV2ObserveBytes), plan: plan, registry: registry, contentType: response.Header.Get("Content-Type"), contentEncoding: response.Header.Get("Content-Encoding"), armed: false, armGeneration: 0}
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
}

func (b *rawResponsesCompactionV2ObservedBody) Read(destination []byte) (int, error) {
	n, err := b.source.Read(destination)
	if n > 0 {
		_, _ = b.captured.Write(destination[:n])
	}
	if err != nil && err != io.EOF {
		return n, fmt.Errorf("read observed compaction response: %w", err)
	}
	if err == nil {
		return n, nil
	}
	return n, io.EOF
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
	body.arm()
}

func (b *rawResponsesCompactionV2ObservedBody) Close() error {
	if err := b.source.Close(); err != nil {
		slog.Warn("adapter.codex.raw_compaction_v2_observer_close_failed", "concern", "adapter.providers.codex.request", "err", err)
		return fmt.Errorf("close observed compaction response: %w", err)
	}
	return nil
}

func (b *rawResponsesCompactionV2ObservedBody) arm() {
	if b.armed || b.captured.Truncated() {
		return
	}
	body := b.captured.Bytes()
	if strings.EqualFold(strings.TrimSpace(b.contentEncoding), "zstd") || strings.EqualFold(strings.TrimSpace(b.contentEncoding), "zstandard") {
		decoder, err := zstd.NewReader(
			bytes.NewReader(body),
			zstd.WithDecoderMaxMemory(maxRawResponsesCompactionV2ObserveBytes),
		)
		if err != nil {
			return
		}
		defer decoder.Close()
		decoded, err := io.ReadAll(io.LimitReader(decoder, maxRawResponsesCompactionV2ObserveBytes+1))
		if err != nil || len(decoded) > maxRawResponsesCompactionV2ObserveBytes {
			return
		}
		body = decoded
	}
	encrypted, ok := rawResponsesCompactionV2EncryptedContent(body, b.contentType)
	if ok {
		generation, armed := b.registry.ArmWithGeneration(b.plan.SessionID, encrypted, b.plan.Transcript)
		if !armed {
			return
		}
		b.armGeneration = generation
		b.armed = true
	}
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
