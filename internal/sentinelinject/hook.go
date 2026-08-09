// Package sentinelinject rewrites intercepted Anthropic /v1/messages responses
// when the latest user message contains a configured keyword.
package sentinelinject

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

const (
	messagesPath            = "/v1/messages"
	claudeProviderName      = "claude"
	eventStreamContentType  = "text/event-stream"
	sentinelInjectConcern   = "providers.mitm.wire"
	sentinelInjectComponent = "mitm"
	defaultTextContentIndex = 0
)

// contentBlockEventName enumerates Anthropic SSE content-block event names the
// rewrite path strips and re-emits.
type contentBlockEventName string

const (
	contentBlockEventStart contentBlockEventName = "content_block_start"
	contentBlockEventDelta contentBlockEventName = "content_block_delta"
	contentBlockEventStop  contentBlockEventName = "content_block_stop"
)

const (
	sseEventMessageStart = "message_start"
)

// Hook scans the latest user message on an Anthropic /v1/messages request and
// rewrites the downstream model text to everything after the configured keyword.
type Hook struct {
	sentinel string
}

// New constructs a sentinel rewrite hook. An empty sentinel yields a hook that
// never matches.
func New(sentinel string) *Hook {
	return &Hook{sentinel: strings.TrimSpace(sentinel)}
}

// MatchRequestResponse pairs a response transformer when the latest user
// message contains the configured keyword.
func (h *Hook) MatchRequestResponse(
	req mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	if h == nil || h.sentinel == "" {
		return unmatchedRequestResponseHookMatch(), nil
	}
	if req.Method != http.MethodPost {
		return unmatchedRequestResponseHookMatch(), nil
	}
	if req.Provider != claudeProviderName {
		return unmatchedRequestResponseHookMatch(), nil
	}
	if req.Path != messagesPath {
		return unmatchedRequestResponseHookMatch(), nil
	}
	body, err := req.Body.Bytes()
	if err != nil {
		slog.Warn(
			"mitm.sentinel_inject.request_body_read_failed",
			"component", sentinelInjectComponent,
			"concern", sentinelInjectConcern,
			"method", req.Method,
			"path", req.Path,
			"err", err,
		)
		return unmatchedRequestResponseHookMatch(), nil
	}
	var request anthropicMessagesRequest
	if err := json.Unmarshal(body, &request); err != nil {
		slog.Warn(
			"mitm.sentinel_inject.request_body_decode_failed",
			"component", sentinelInjectComponent,
			"concern", sentinelInjectConcern,
			"method", req.Method,
			"path", req.Path,
			"err", err,
		)
		return unmatchedRequestResponseHookMatch(), nil
	}
	forced, ok := extractForcedContent(lastUserMessageText(request), h.sentinel)
	if !ok {
		return unmatchedRequestResponseHookMatch(), nil
	}
	return mitm.RequestResponseHookMatch{
		Matched: true,
		Transformer: responseReplaceTransformer{
			content: forced,
		},
		RequestTransformer: nil,
	}, nil
}

func unmatchedRequestResponseHookMatch() mitm.RequestResponseHookMatch {
	return mitm.RequestResponseHookMatch{
		Matched:            false,
		Transformer:        nil,
		RequestTransformer: nil,
	}
}

type anthropicMessagesRequest struct {
	Messages []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func lastUserMessageText(request anthropicMessagesRequest) string {
	for _, message := range slices.Backward(request.Messages) {
		if message.Role == "user" {
			return message.text()
		}
	}
	return ""
}

func (m anthropicMessage) text() string {
	trimmed := strings.TrimSpace(string(m.Content))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err != nil {
			return ""
		}
		return plain
	}
	var blocks []anthropicTextBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, block := range blocks {
		if block.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(block.Text)
	}
	return builder.String()
}

func extractForcedContent(userText, sentinel string) (string, bool) {
	if sentinel == "" {
		return "", false
	}
	_, after, found := strings.Cut(userText, sentinel)
	if !found {
		return "", false
	}
	return after, true
}

type responseReplaceTransformer struct {
	content string
}

func (t responseReplaceTransformer) TransformResponse(
	ctx context.Context,
	resp mitm.ResponseHookResponse,
) (mitm.ResponseHookResponse, error) {
	if !responseIsStreamingSuccess(resp) {
		return resp, nil
	}
	slog.InfoContext(
		ctx,
		"mitm.sentinel_inject.matched",
		"component", sentinelInjectComponent,
		"concern", sentinelInjectConcern,
	)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.WarnContext(
			ctx,
			"mitm.sentinel_inject.response_body_read_failed",
			"component", sentinelInjectComponent,
			"concern", sentinelInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	output, err := replaceSSEText(body, t.content)
	if err != nil {
		slog.WarnContext(
			ctx,
			"mitm.sentinel_inject.sse_replace_failed",
			"component", sentinelInjectComponent,
			"concern", sentinelInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	return responseWithBody(resp, output), nil
}

func responseIsStreamingSuccess(resp mitm.ResponseHookResponse) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(contentType, eventStreamContentType)
}

func replaceSSEText(body []byte, forced string) ([]byte, error) {
	events := parseSSEEvents(string(body))
	if len(events) == 0 {
		return body, nil
	}
	output := make([]sseEvent, 0, len(events)+3)
	blockInserted := false
	for _, event := range events {
		if isContentBlockEvent(event.Name) {
			continue
		}
		output = append(output, event)
		if event.Name == sseEventMessageStart && !blockInserted {
			appendEvents, err := marshalTextBlockEvents(defaultTextContentIndex, forced)
			if err != nil {
				return nil, err
			}
			output = append(output, appendEvents...)
			blockInserted = true
		}
	}
	if !blockInserted {
		return body, nil
	}
	return buildSSEBody(output), nil
}

func isContentBlockEvent(name string) bool {
	switch contentBlockEventName(name) {
	case contentBlockEventStart, contentBlockEventDelta, contentBlockEventStop:
		return true
	default:
		return false
	}
}

func marshalTextBlockEvents(blockIndex int, content string) ([]sseEvent, error) {
	start, err := MarshalSSEData(newContentBlockStartPayload(blockIndex))
	if err != nil {
		return nil, err
	}
	delta, err := MarshalSSEData(newContentBlockDeltaPayload(blockIndex, content))
	if err != nil {
		return nil, err
	}
	stop, err := MarshalSSEData(newContentBlockStopPayload(blockIndex))
	if err != nil {
		return nil, err
	}
	return []sseEvent{
		{Name: string(contentBlockEventStart), Data: start},
		{Name: string(contentBlockEventDelta), Data: delta},
		{Name: string(contentBlockEventStop), Data: stop},
	}, nil
}

func responseWithBody(
	resp mitm.ResponseHookResponse,
	body []byte,
) mitm.ResponseHookResponse {
	header := resp.Header.Clone()
	header.Del("Content-Length")
	return mitm.ResponseHookResponse{
		StatusCode:    resp.StatusCode,
		Status:        resp.Status,
		Proto:         resp.Proto,
		Header:        header,
		Body:          bytes.NewReader(body),
		ContentLength: -1,
	}
}

type sseEvent struct {
	Name string
	Data string
}

type ssePayload interface {
	contentBlockStartPayload | contentBlockDeltaPayload | contentBlockStopPayload
}

type textContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type contentBlockStartPayload struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	ContentBlock textContentBlock `json:"content_block"`
}

type contentBlockDeltaPayload struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta textDelta `json:"delta"`
}

type contentBlockStopPayload struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

func newContentBlockStartPayload(blockIndex int) contentBlockStartPayload {
	return contentBlockStartPayload{
		Type:  string(contentBlockEventStart),
		Index: blockIndex,
		ContentBlock: textContentBlock{
			Type: "text",
			Text: "",
		},
	}
}

func newContentBlockDeltaPayload(blockIndex int, content string) contentBlockDeltaPayload {
	return contentBlockDeltaPayload{
		Type:  string(contentBlockEventDelta),
		Index: blockIndex,
		Delta: textDelta{
			Type: "text_delta",
			Text: content,
		},
	}
}

func newContentBlockStopPayload(blockIndex int) contentBlockStopPayload {
	return contentBlockStopPayload{
		Type:  string(contentBlockEventStop),
		Index: blockIndex,
	}
}

// MarshalSSEData encodes one Anthropic SSE JSON payload without HTML escaping.
func MarshalSSEData[T ssePayload](payload T) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		slog.Warn(
			"mitm.sentinel_inject.sse_json_encode_failed",
			"component", sentinelInjectComponent,
			"concern", sentinelInjectConcern,
			"err", err,
		)
		return "", fmt.Errorf("encode sentinel inject SSE JSON: %w", err)
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

func parseSSEEvents(body string) []sseEvent {
	records := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	events := make([]sseEvent, 0, len(records))
	for _, record := range records {
		event := parseSSERecord(record)
		if event.Name == "" && event.Data == "" {
			continue
		}
		events = append(events, event)
	}
	return events
}

func parseSSERecord(record string) sseEvent {
	lines := strings.Split(record, "\n")
	dataLines := make([]string, 0, len(lines))
	event := sseEvent{Name: "", Data: ""}
	for _, line := range lines {
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event.Name = strings.TrimSpace(value)
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		}
	}
	event.Data = strings.Join(dataLines, "\n")
	return event
}

func buildSSEBody(events []sseEvent) []byte {
	var builder strings.Builder
	for _, event := range events {
		if event.Name != "" {
			builder.WriteString("event: ")
			builder.WriteString(event.Name)
			builder.WriteByte('\n')
		}
		writeSSEDataLines(&builder, event.Data)
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func writeSSEDataLines(builder *strings.Builder, data string) {
	if data == "" {
		return
	}
	for line := range strings.SplitSeq(data, "\n") {
		builder.WriteString("data: ")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
}
