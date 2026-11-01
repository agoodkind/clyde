package codex

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

func appendRawCompactionAssistantContentPart(
	item []byte,
	contentStart int,
	contentEnd int,
	hasContent bool,
	transcriptText string,
) ([]byte, bool, bool) {
	partIndex := 0
	if hasContent {
		ranges, ok := jsonArrayValueRanges(item[contentStart:contentEnd])
		if !ok {
			return item, false, false
		}
		partIndex = len(ranges)
	}
	encodedText, ok := marshalRawCompactionString(transcriptText)
	if !ok {
		return item, false, false
	}
	part := append([]byte(`{"type":"output_text","annotations":[],"logprobs":[],"text":`), encodedText...)
	part = append(part, '}')
	var appended []byte
	if hasContent {
		appended, ok = appendRawCompactionContentArrayPart(item, contentStart, contentEnd, part)
	} else {
		appended, ok = appendRawCompactionObjectField(item, "content", append(append([]byte{'['}, part...), ']'))
	}
	if !ok {
		return item, false, false
	}
	marked, ok := markRawCompactionTranscriptPart(appended, partIndex)
	if !ok {
		return item, false, false
	}
	return marked, true, true
}

func appendRawCompactionContentArrayPart(item []byte, contentStart int, contentEnd int, part []byte) ([]byte, bool) {
	content := item[contentStart:contentEnd]
	closing := len(content) - 1
	for closing >= 0 && (content[closing] == ' ' || content[closing] == '\t' || content[closing] == '\r' || content[closing] == '\n') {
		closing--
	}
	if closing < 0 || content[closing] != ']' {
		return item, false
	}
	separator := rawCompactionMemberSeparator(content[1:closing])
	replacement := slices.Concat(content[:closing], separator, part, content[closing:])
	return replaceByteRange(item, contentStart, contentEnd, replacement), true
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
