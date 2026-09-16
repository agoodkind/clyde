package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

func rawCompactionJSONHasUniqueObjectKeys(raw []byte) bool {
	if !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if !rawCompactionDecodeUniqueJSONValue(decoder) {
		return false
	}
	var trailing json.RawMessage
	return decoder.Decode(&trailing) == io.EOF
}

func rawCompactionDecodeUniqueJSONValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return true
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return false
			}
			if _, duplicate := keys[key]; duplicate {
				return false
			}
			keys[key] = struct{}{}
			if !rawCompactionDecodeUniqueJSONValue(decoder) {
				return false
			}
		}
	case '[':
		for decoder.More() {
			if !rawCompactionDecodeUniqueJSONValue(decoder) {
				return false
			}
		}
	default:
		return false
	}
	_, err = decoder.Token()
	return err == nil
}

func rawCompactionStrictFinalAnswerJSON(body []byte) bool {
	if !rawCompactionJSONHasUniqueObjectKeys(body) {
		return false
	}
	var response struct {
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
	}
	if json.Unmarshal(body, &response) != nil || response.Status != "completed" {
		return false
	}
	return rawCompactionStrictFinalAnswerOutput(response.Output)
}

func rawCompactionStrictFinalAnswerSSEFrame(frame []byte) bool {
	_, data, dataCount := rawSSEFrameDataValue(frame)
	if dataCount != 1 || !rawCompactionJSONHasUniqueObjectKeys(data) {
		return false
	}
	var payload struct {
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Response) == 0 {
		return false
	}
	return rawCompactionStrictFinalAnswerJSON(payload.Response)
}

func rawCompactionStrictFinalAnswerOutput(output []json.RawMessage) bool {
	found := false
	for _, rawItem := range output {
		var item map[string]json.RawMessage
		if json.Unmarshal(rawItem, &item) != nil {
			return false
		}
		var itemType string
		if json.Unmarshal(item["type"], &itemType) != nil {
			return false
		}
		if itemType == "reasoning" {
			continue
		}
		if itemType != "message" {
			return false
		}
		var role, phase string
		if json.Unmarshal(item["role"], &role) != nil || json.Unmarshal(item["phase"], &phase) != nil ||
			role != "assistant" || phase != "final_answer" {
			return false
		}
		content, ok := item["content"]
		if !ok || !rawCompactionStrictFinalAnswerContent(content) {
			return false
		}
		if found {
			return false
		}
		found = true
	}
	return found
}

func rawCompactionStrictFinalAnswerContent(raw json.RawMessage) bool {
	var content []json.RawMessage
	if json.Unmarshal(raw, &content) != nil || len(content) != 1 {
		return false
	}
	var part map[string]json.RawMessage
	if json.Unmarshal(content[0], &part) != nil {
		return false
	}
	var partType, text string
	if json.Unmarshal(part["type"], &partType) != nil || json.Unmarshal(part["text"], &text) != nil {
		return false
	}
	if partType != "output_text" || rawCompactionHasCompleteTranscriptWrapper(text) {
		return false
	}
	for key := range part {
		if strings.EqualFold(key, "type") && key != "type" || strings.EqualFold(key, "text") && key != "text" {
			return false
		}
	}
	return true
}

func rawCompactionHasCompleteTranscriptWrapper(text string) bool {
	const openTag = "<pre-compaction-transcript>"
	const closeTag = "</pre-compaction-transcript>"
	openIndex := strings.Index(text, openTag)
	for openIndex >= 0 {
		closeIndex := strings.Index(text[openIndex+len(openTag):], closeTag)
		if closeIndex >= 0 {
			return true
		}
		nextOffset := openIndex + len(openTag)
		nextIndex := strings.Index(text[nextOffset:], openTag)
		if nextIndex < 0 {
			return false
		}
		openIndex = nextOffset + nextIndex
	}
	return false
}
