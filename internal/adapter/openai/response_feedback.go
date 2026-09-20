package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

const responseFeedbackEventCount = 6

type responseSSEEvent struct {
	name string
	data []byte
	raw  string
}

type responseEventIdentity struct {
	Type           string `json:"type"`
	SequenceNumber *int   `json:"sequence_number"`
	OutputIndex    *int   `json:"output_index"`
}

type responseTextDelta struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
}

type responseMessageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type responseOutputItemDone struct {
	Type string              `json:"type"`
	Item responseMessageItem `json:"item"`
}

type responseCompleted struct {
	Type     string `json:"type"`
	Response struct {
		Output []responseMessageItem `json:"output"`
	} `json:"response"`
}

// ResponseText returns assistant output text from a Responses SSE stream.
func ResponseText(body []byte) string {
	events := parseResponseSSE(body)
	var deltas strings.Builder
	for _, event := range events {
		var payload responseTextDelta
		if json.Unmarshal(event.data, &payload) != nil || payload.Type != ResponsesEventOutputTextDelta {
			continue
		}
		deltas.WriteString(payload.Delta)
	}
	if deltas.Len() > 0 {
		return deltas.String()
	}
	var doneItems []responseMessageItem
	var terminalItems []responseMessageItem
	for _, event := range events {
		var done responseOutputItemDone
		if json.Unmarshal(event.data, &done) == nil && done.Type == ResponsesEventOutputItemDone {
			doneItems = append(doneItems, done.Item)
		}
		var completed responseCompleted
		if json.Unmarshal(event.data, &completed) == nil && completed.Type == ResponsesEventCompleted {
			terminalItems = completed.Response.Output
		}
	}
	if len(terminalItems) > 0 {
		return responseItemsText(terminalItems)
	}
	return responseItemsText(doneItems)
}

func responseItemsText(items []responseMessageItem) string {
	var builder strings.Builder
	for _, item := range items {
		if item.Type != "message" || item.Role != "assistant" {
			continue
		}
		for _, content := range item.Content {
			if content.Type != "output_text" || content.Text == "" {
				continue
			}
			if builder.Len() > 0 {
				builder.WriteByte('\n')
			}
			builder.WriteString(content.Text)
		}
	}
	return builder.String()
}

// AppendTextItem inserts one assistant message before response.completed.
func AppendTextItem(body []byte, text string) ([]byte, error) {
	events := parseResponseSSE(body)
	completedIndex := -1
	maxSequence := -1
	maxOutputIndex := -1
	for index, event := range events {
		var identity responseEventIdentity
		if json.Unmarshal(event.data, &identity) != nil {
			continue
		}
		if identity.OutputIndex != nil && *identity.OutputIndex > maxOutputIndex {
			maxOutputIndex = *identity.OutputIndex
		}
		if identity.Type == ResponsesEventCompleted {
			completedIndex = index
			if outputCount := completedResponseOutputCount(event.data); outputCount-1 > maxOutputIndex {
				maxOutputIndex = outputCount - 1
			}
			continue
		}
		if identity.SequenceNumber != nil && *identity.SequenceNumber > maxSequence {
			maxSequence = *identity.SequenceNumber
		}
	}
	if completedIndex < 0 {
		return nil, fmt.Errorf("responses stream has no response.completed event")
	}
	outputIndex := maxOutputIndex + 1
	itemID := fmt.Sprintf("msg_clyde_response_%d", outputIndex)
	item := ResponsesOutputItem{
		Type:      "message",
		ID:        itemID,
		Status:    ResponsesOutputItemStatusCompleted,
		Role:      "assistant",
		Content:   []ResponsesContentPart{{Type: "output_text", Text: text, Refusal: "", Annotations: nil}},
		Summary:   nil,
		CallID:    "",
		Name:      "",
		Arguments: "",
	}
	synthetic, err := writeResponseFeedbackEvents(item, outputIndex, maxSequence+1)
	if err != nil {
		return nil, err
	}
	completedData, err := writeCompletedResponseItem(events[completedIndex].data, item, maxSequence+responseFeedbackEventCount+1)
	if err != nil {
		return nil, err
	}
	events[completedIndex].data = completedData
	events[completedIndex].raw = ""
	result := make([]responseSSEEvent, 0, len(events)+len(synthetic))
	result = append(result, events[:completedIndex]...)
	result = append(result, synthetic...)
	result = append(result, events[completedIndex:]...)
	return buildResponseSSE(result), nil
}

func completedResponseOutputCount(data []byte) int {
	var completed responseCompleted
	if json.Unmarshal(data, &completed) != nil || completed.Type != ResponsesEventCompleted {
		return 0
	}
	return len(completed.Response.Output)
}

func writeResponseFeedbackEvents(
	item ResponsesOutputItem,
	outputIndex int,
	firstSequence int,
) ([]responseSSEEvent, error) {
	emptyItem := item
	emptyItem.Status = ResponsesOutputItemStatusInProgress
	emptyItem.Content = []ResponsesContentPart{}
	emptyPart := ResponsesContentPart{Type: "output_text", Text: "", Refusal: "", Annotations: nil}
	completePart := item.Content[0]
	events := []ResponsesStreamEvent{
		ResponsesOutputItemEvent{Type: ResponsesEventOutputItemAdded, OutputIndex: outputIndex, Item: emptyItem, SequenceNumber: firstSequence},
		ResponsesContentPartEvent{Type: ResponsesEventContentPartAdded, ItemID: item.ID, OutputIndex: outputIndex, ContentIndex: 0, Part: emptyPart, SequenceNumber: firstSequence + 1},
		ResponsesOutputTextDeltaEvent{Type: ResponsesEventOutputTextDelta, ItemID: item.ID, OutputIndex: outputIndex, ContentIndex: 0, Delta: completePart.Text, SequenceNumber: firstSequence + 2},
		ResponsesOutputTextDoneEvent{Type: ResponsesEventOutputTextDone, ItemID: item.ID, OutputIndex: outputIndex, ContentIndex: 0, Text: completePart.Text, SequenceNumber: firstSequence + 3},
		ResponsesContentPartEvent{Type: ResponsesEventContentPartDone, ItemID: item.ID, OutputIndex: outputIndex, ContentIndex: 0, Part: completePart, SequenceNumber: firstSequence + 4},
		ResponsesOutputItemEvent{Type: ResponsesEventOutputItemDone, OutputIndex: outputIndex, Item: item, SequenceNumber: firstSequence + 5},
	}
	result := make([]responseSSEEvent, 0, len(events))
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			slog.Warn("adapter.openai.response_feedback_encode_failed", "concern", "providers.mitm.wire", "err", err)
			return nil, fmt.Errorf("encode Responses feedback event: %w", err)
		}
		var identity responseEventIdentity
		if err := json.Unmarshal(data, &identity); err != nil {
			slog.Warn("adapter.openai.response_feedback_decode_failed", "concern", "providers.mitm.wire", "err", err)
			return nil, fmt.Errorf("read Responses feedback event type: %w", err)
		}
		result = append(result, responseSSEEvent{name: identity.Type, data: data, raw: ""})
	}
	return result, nil
}

func writeCompletedResponseItem(
	data []byte,
	item ResponsesOutputItem,
	sequenceNumber int,
) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		slog.Warn("adapter.openai.response_feedback_decode_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("decode response.completed event: %w", err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(envelope["response"], &response); err != nil {
		slog.Warn("adapter.openai.response_feedback_decode_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("decode response.completed object: %w", err)
	}
	var output []json.RawMessage
	if rawOutput := response["output"]; len(rawOutput) > 0 && !bytes.Equal(bytes.TrimSpace(rawOutput), []byte("null")) {
		if err := json.Unmarshal(rawOutput, &output); err != nil {
			slog.Warn("adapter.openai.response_feedback_decode_failed", "concern", "providers.mitm.wire", "err", err)
			return nil, fmt.Errorf("decode response.completed output: %w", err)
		}
	}
	itemData, err := json.Marshal(item)
	if err != nil {
		slog.Warn("adapter.openai.response_feedback_encode_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("encode Responses feedback item: %w", err)
	}
	output = append(output, itemData)
	response["output"], err = json.Marshal(output)
	if err != nil {
		slog.Warn("adapter.openai.response_feedback_encode_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("encode response.completed output: %w", err)
	}
	envelope["response"], err = json.Marshal(response)
	if err != nil {
		slog.Warn("adapter.openai.response_feedback_encode_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("encode response.completed object: %w", err)
	}
	envelope["sequence_number"], err = json.Marshal(sequenceNumber)
	if err != nil {
		slog.Warn("adapter.openai.response_feedback_encode_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("encode response.completed sequence: %w", err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		slog.Warn("adapter.openai.response_feedback_encode_failed", "concern", "providers.mitm.wire", "err", err)
		return nil, fmt.Errorf("encode response.completed event: %w", err)
	}
	return encoded, nil
}

func parseResponseSSE(body []byte) []responseSSEEvent {
	normalized := strings.ReplaceAll(string(body), "\r\n", "\n")
	records := strings.Split(normalized, "\n\n")
	events := make([]responseSSEEvent, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record) == "" {
			continue
		}
		event := responseSSEEvent{name: "", data: nil, raw: record}
		var dataLines []string
		for line := range strings.SplitSeq(record, "\n") {
			if value, ok := strings.CutPrefix(line, "event:"); ok {
				event.name = strings.TrimSpace(value)
				continue
			}
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				dataLines = append(dataLines, strings.TrimPrefix(value, " "))
			}
		}
		event.data = []byte(strings.Join(dataLines, "\n"))
		events = append(events, event)
	}
	return events
}

func buildResponseSSE(events []responseSSEEvent) []byte {
	var builder strings.Builder
	for _, event := range events {
		if event.raw != "" {
			builder.WriteString(event.raw)
			builder.WriteString("\n\n")
			continue
		}
		if event.name != "" {
			builder.WriteString("event: ")
			builder.WriteString(event.name)
			builder.WriteByte('\n')
		}
		if len(event.data) > 0 {
			for line := range strings.SplitSeq(string(event.data), "\n") {
				builder.WriteString("data: ")
				builder.WriteString(line)
				builder.WriteByte('\n')
			}
		}
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}
