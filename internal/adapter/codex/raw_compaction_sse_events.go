package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

const rawCompactionSSESyntheticEventCount = 4

type rawCompactionSSEContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rawCompactionSSESyntheticContentPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
	Logprobs    []json.RawMessage `json:"logprobs"`
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
		if rawCompactionTranscriptPresent(text, transcriptText) {
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
	emptyPart := &rawCompactionSSESyntheticContentPart{Type: "output_text", Text: "", Annotations: emptyAnnotations, Logprobs: emptyLogprobs}
	completePart := &rawCompactionSSESyntheticContentPart{Type: "output_text", Text: transcriptText, Annotations: emptyAnnotations, Logprobs: emptyLogprobs}
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
	if bytes.Equal(bytes.TrimSpace(data[start:end]), []byte("null")) {
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
	if bytes.Equal(bytes.TrimSpace(data[start:end]), []byte("null")) {
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
