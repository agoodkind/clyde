package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
)

func appendRawCompactionJSON(body []byte, transcriptText string) ([]byte, bool) {
	outputStart, outputEnd, ok := jsonObjectFieldValueRange(body, "output")
	if !ok {
		return body, false
	}
	ranges, ok := jsonArrayValueRanges(body[outputStart:outputEnd])
	if !ok {
		return body, false
	}
	for index := range slices.Backward(ranges) {
		itemStart := outputStart + ranges[index].start
		itemEnd := outputStart + ranges[index].end
		mutated, matched, valid := appendRawCompactionAssistantItem(body[itemStart:itemEnd], transcriptText)
		if !valid {
			return body, false
		}
		if !matched {
			continue
		}
		if bytes.Equal(mutated, body[itemStart:itemEnd]) {
			return body, true
		}
		return replaceByteRange(body, itemStart, itemEnd, mutated), true
	}
	return body, false
}

func appendRawCompactionAssistantItem(
	item []byte,
	transcriptText string,
) ([]byte, bool, bool) {
	var identity struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(item, &identity) != nil {
		return item, false, false
	}
	if identity.Type != "message" || identity.Role != "assistant" {
		return item, false, true
	}
	contentStart, contentEnd, hasContent := jsonObjectFieldValueRange(item, "content")
	if !hasContent {
		return appendRawCompactionAssistantContentPart(item, 0, 0, false, transcriptText)
	}
	contentRanges, ok := jsonArrayValueRanges(item[contentStart:contentEnd])
	if !ok {
		return item, false, false
	}
	var target rawCompactionInterval
	hasTarget := false
	for index := range slices.Backward(contentRanges) {
		partStart := contentStart + contentRanges[index].start
		partEnd := contentStart + contentRanges[index].end
		part := item[partStart:partEnd]
		var contentIdentity struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(part, &contentIdentity) != nil {
			return item, false, false
		}
		if contentIdentity.Type != "output_text" {
			continue
		}
		textStart, textEnd, ok := jsonObjectFieldValueRange(part, "text")
		if !ok {
			return item, false, false
		}
		var text string
		if bytes.Equal(bytes.TrimSpace(part[textStart:textEnd]), []byte("null")) ||
			json.Unmarshal(part[textStart:textEnd], &text) != nil {
			return item, false, false
		}
		if rawCompactionTranscriptPresent(text, transcriptText) {
			return item, true, true
		}
		if !hasTarget {
			target = rawCompactionInterval{start: partStart, end: partEnd}
			hasTarget = true
		}
	}
	if !hasTarget {
		return appendRawCompactionAssistantContentPart(item, contentStart, contentEnd, true, transcriptText)
	}
	part := item[target.start:target.end]
	textStart, textEnd, ok := jsonObjectFieldValueRange(part, "text")
	if !ok {
		return item, false, false
	}
	var text string
	if bytes.Equal(bytes.TrimSpace(part[textStart:textEnd]), []byte("null")) ||
		json.Unmarshal(part[textStart:textEnd], &text) != nil {
		return item, false, false
	}
	encodedText, ok := marshalRawCompactionString(text + transcriptText)
	if !ok {
		return item, false, false
	}
	mutatedPart := replaceByteRange(part, textStart, textEnd, encodedText)
	return replaceByteRange(item, target.start, target.end, mutatedPart), true, true
}

func rawCompactionTranscriptPresent(text, transcriptText string) bool {
	needle := strings.TrimSpace(transcriptText)
	if needle == "" {
		return false
	}
	searchStart := 0
	for searchStart < len(text) {
		matchOffset := strings.Index(text[searchStart:], needle)
		if matchOffset < 0 {
			return false
		}
		matchStart := searchStart + matchOffset
		matchEnd := matchStart + len(needle)
		beforeBoundary := matchStart == 0 || rawCompactionTranscriptBoundary(text[matchStart-1])
		afterBoundary := matchEnd == len(text) || rawCompactionTranscriptBoundary(text[matchEnd])
		if beforeBoundary && afterBoundary {
			return true
		}
		searchStart = matchStart + 1
	}
	return false
}

func rawCompactionTranscriptBoundary(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func marshalRawCompactionString(value string) ([]byte, bool) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, false
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), true
}

func marshalRawArray(items []json.RawMessage) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('[')
	for index, item := range items {
		if !json.Valid(item) {
			return nil, errors.New("raw compaction input item is invalid JSON")
		}
		if index > 0 {
			buffer.WriteByte(',')
		}
		buffer.Write(item)
	}
	buffer.WriteByte(']')
	return buffer.Bytes(), nil
}

func jsonObjectFieldValueRange(raw []byte, field string) (int, int, bool) {
	if !json.Valid(raw) {
		return 0, 0, false
	}
	index := skipJSONSpace(raw, 0)
	if index >= len(raw) || raw[index] != '{' {
		return 0, 0, false
	}
	index++
	for {
		index = skipJSONSpace(raw, index)
		if index >= len(raw) || raw[index] == '}' {
			return 0, 0, false
		}
		keyStart := index
		keyEnd, ok := scanJSONStringEnd(raw, keyStart)
		if !ok {
			return 0, 0, false
		}
		key, err := strconv.Unquote(string(raw[keyStart:keyEnd]))
		if err != nil {
			return 0, 0, false
		}
		index = skipJSONSpace(raw, keyEnd)
		if index >= len(raw) || raw[index] != ':' {
			return 0, 0, false
		}
		valueStart := skipJSONSpace(raw, index+1)
		valueEnd, ok := scanJSONValueEnd(raw, valueStart)
		if !ok {
			return 0, 0, false
		}
		if key == field {
			return valueStart, valueEnd, true
		}
		index = skipJSONSpace(raw, valueEnd)
		if index < len(raw) && raw[index] == ',' {
			index++
			continue
		}
		return 0, 0, false
	}
}

func jsonArrayValueRanges(raw []byte) ([]rawCompactionInterval, bool) {
	if !json.Valid(raw) {
		return nil, false
	}
	index := skipJSONSpace(raw, 0)
	if index >= len(raw) || raw[index] != '[' {
		return nil, false
	}
	index++
	ranges := make([]rawCompactionInterval, 0)
	for {
		index = skipJSONSpace(raw, index)
		if index >= len(raw) {
			return nil, false
		}
		if raw[index] == ']' {
			return ranges, true
		}
		valueEnd, ok := scanJSONValueEnd(raw, index)
		if !ok {
			return nil, false
		}
		ranges = append(ranges, rawCompactionInterval{start: index, end: valueEnd})
		index = skipJSONSpace(raw, valueEnd)
		if index < len(raw) && raw[index] == ',' {
			index++
			continue
		}
		if index < len(raw) && raw[index] == ']' {
			return ranges, true
		}
		return nil, false
	}
}

func scanJSONValueEnd(raw []byte, start int) (int, bool) {
	if start >= len(raw) {
		return 0, false
	}
	switch raw[start] {
	case '"':
		return scanJSONStringEnd(raw, start)
	case '{', '[':
		return scanJSONContainerEnd(raw, start)
	default:
		return scanJSONPrimitiveEnd(raw, start)
	}
}

func scanJSONContainerEnd(raw []byte, start int) (int, bool) {
	opening := raw[start]
	closing := byte('}')
	if opening == '[' {
		closing = ']'
	}
	depth := 0
	for index := start; index < len(raw); index++ {
		if raw[index] == '"' {
			stringEnd, ok := scanJSONStringEnd(raw, index)
			if !ok {
				return 0, false
			}
			index = stringEnd - 1
			continue
		}
		if raw[index] == opening {
			depth++
			continue
		}
		if raw[index] != closing {
			continue
		}
		depth--
		if depth == 0 {
			return index + 1, true
		}
	}
	return 0, false
}

func scanJSONPrimitiveEnd(raw []byte, start int) (int, bool) {
	index := start
	for index < len(raw) && !strings.ContainsRune(",}] \t\r\n", rune(raw[index])) {
		index++
	}
	return index, index > start
}

func scanJSONStringEnd(raw []byte, start int) (int, bool) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, false
	}
	escaped := false
	for index := start + 1; index < len(raw); index++ {
		if escaped {
			escaped = false
			continue
		}
		if raw[index] == '\\' {
			escaped = true
			continue
		}
		if raw[index] == '"' {
			return index + 1, true
		}
	}
	return 0, false
}

func skipJSONSpace(raw []byte, index int) int {
	for index < len(raw) {
		switch raw[index] {
		case ' ', '\t', '\r', '\n':
			index++
		default:
			return index
		}
	}
	return index
}

func replaceByteRange(raw []byte, start, end int, replacement []byte) []byte {
	out := make([]byte, 0, len(raw))
	out = append(out, raw[:start]...)
	out = append(out, replacement...)
	out = append(out, raw[end:]...)
	return out
}
