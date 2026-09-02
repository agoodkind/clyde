package codex

import (
	"bytes"
	"encoding/json"
	"slices"
)

func appendRawCompactionJSON(body []byte, transcriptText string, requireCompletedStatus bool) ([]byte, bool) {
	if requireCompletedStatus {
		statusStart, statusEnd, ok := jsonObjectFieldValueRange(body, "status")
		if !ok {
			return body, false
		}
		var status string
		if json.Unmarshal(body[statusStart:statusEnd], &status) != nil || status != "completed" {
			return body, false
		}
	}
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
