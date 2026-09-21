package codex

import (
	"bytes"
	"encoding/json"
	"strings"
)

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
	part := append([]byte(`{"type":"output_text","annotations":[],"logprobs":[],"text":`), encodedText...)
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
	for line := range bytes.SplitSeq(rawSSENormalizeLineEndings(frame), []byte("\n")) {
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
	for line := range bytes.SplitSeq(rawSSENormalizeLineEndings(frame), []byte("\n")) {
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

func rawSSENormalizeLineEndings(frame []byte) []byte {
	normalized := make([]byte, 0, len(frame))
	for index := 0; index < len(frame); index++ {
		if frame[index] == '\r' {
			if index+1 < len(frame) && frame[index+1] == '\n' {
				index++
			}
			normalized = append(normalized, '\n')
			continue
		}
		normalized = append(normalized, frame[index])
	}
	return normalized
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
	for _, lineRange := range rawSSEFrameLineRanges(frame) {
		lineStart := lineRange.start
		lineEnd := lineRange.end
		line := frame[lineStart:lineEnd]
		lineContent := frame[lineRange.start:lineRange.contentEnd]
		field, _ := rawSSEField(lineContent)
		if !bytes.Equal(field, []byte("data")) {
			result.Write(line)
			continue
		}
		if firstDataLineStart < 0 {
			firstDataLineStart = lineStart
			result.WriteString("data: ")
			result.Write(data)
			result.Write(line[lineRange.contentEnd-lineStart:])
		}
	}
	return result.Bytes()
}

type rawSSEFrameLineRange struct {
	start      int
	contentEnd int
	end        int
}

func rawSSEFrameLineRanges(frame []byte) []rawSSEFrameLineRange {
	ranges := make([]rawSSEFrameLineRange, 0)
	for start := 0; start < len(frame); {
		contentEnd := start
		for contentEnd < len(frame) && frame[contentEnd] != '\n' && frame[contentEnd] != '\r' {
			contentEnd++
		}
		end := contentEnd
		if end < len(frame) {
			end++
			if frame[contentEnd] == '\r' && end < len(frame) && frame[end] == '\n' {
				end++
			}
		}
		ranges = append(ranges, rawSSEFrameLineRange{start: start, contentEnd: contentEnd, end: end})
		start = end
	}
	return ranges
}
