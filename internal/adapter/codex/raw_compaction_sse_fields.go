package codex

import (
	"bytes"
	"encoding/json"
	"strings"
)

func selectRawCompactionStart(
	units []rawCompactionInterval,
	maxBytes int,
	targetCount int,
	render func(start int) (string, bool),
) (int, string, bool) {
	firstCandidate := len(units) - targetCount
	if maxBytes <= 0 {
		rendered, ok := render(units[firstCandidate].start)
		return firstCandidate, rendered, ok && strings.TrimSpace(rendered) != ""
	}

	selected := -1
	selectedTranscript := ""
	lower := firstCandidate
	upper := len(units) - 1
	for lower <= upper {
		middle := lower + (upper-lower)/2
		rendered, ok := render(units[middle].start)
		if !ok || strings.TrimSpace(rendered) == "" {
			return 0, "", false
		}
		if len(rendered) > maxBytes {
			lower = middle + 1
			continue
		}
		selected = middle
		selectedTranscript = rendered
		upper = middle - 1
	}
	if selected < 0 {
		return 0, "", false
	}
	return selected, selectedTranscript, true
}

func appendRawCompactionAssistantContentPart(
	item []byte,
	contentStart int,
	contentEnd int,
	hasContent bool,
	transcriptText string,
) ([]byte, bool, bool) {
	encodedText, ok := marshalRawCompactionString(transcriptText)
	if !ok {
		return item, false, false
	}
	part := append([]byte(`{"type":"output_text","text":`), encodedText...)
	part = append(part, '}')
	if hasContent {
		content := item[contentStart:contentEnd]
		closing := len(content) - 1
		for closing >= 0 && (content[closing] == ' ' || content[closing] == '\t' || content[closing] == '\r' || content[closing] == '\n') {
			closing--
		}
		if closing < 0 || content[closing] != ']' {
			return item, false, false
		}
		hasParts := len(bytes.TrimSpace(content[1:closing])) > 0
		replacement := make([]byte, 0, len(content)+len(part)+1)
		replacement = append(replacement, content[:closing]...)
		if hasParts {
			replacement = append(replacement, ',')
		}
		replacement = append(replacement, part...)
		replacement = append(replacement, content[closing:]...)
		return replaceByteRange(item, contentStart, contentEnd, replacement), true, true
	}
	closing := len(item) - 1
	for closing >= 0 && (item[closing] == ' ' || item[closing] == '\t' || item[closing] == '\r' || item[closing] == '\n') {
		closing--
	}
	if closing < 0 || item[closing] != '}' {
		return item, false, false
	}
	hasFields := len(bytes.TrimSpace(item[1:closing])) > 0
	replacement := make([]byte, 0, len(item)+len(part)+13)
	replacement = append(replacement, item[:closing]...)
	if hasFields {
		replacement = append(replacement, ',')
	}
	replacement = append(replacement, `"content":[`...)
	replacement = append(replacement, part...)
	replacement = append(replacement, ']')
	replacement = append(replacement, item[closing:]...)
	return replacement, true, true
}

func rawSSEFrameEvent(frame []byte) rawCompactionSSEEvent {
	var eventName rawCompactionSSEEvent
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = rawCompactionSSEEvent(strings.TrimSpace(string(line[len("event:"):])))
		}
	}
	return eventName
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
