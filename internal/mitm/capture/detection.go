package capture

import (
	"bytes"
	"encoding/json"
	"strings"
)

func containsUnredactableSensitiveJSON(raw []byte, sensitiveValues []string) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}

	switch trimmed[0] {
	case '"':
		return containsUnredactableSensitiveJSONString(trimmed, sensitiveValues)
	case '{':
		return containsUnredactableSensitiveJSONObject(trimmed, sensitiveValues)
	case '[':
		return containsUnredactableSensitiveJSONArray(trimmed, sensitiveValues)
	}

	return false
}

func containsUnredactableSensitiveJSONString(raw []byte, sensitiveValues []string) bool {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return true
	}

	if containsSensitiveBodyMarker([]byte(value)) {
		return true
	}

	return bytes.Contains(raw, []byte(`\u`)) && containsSensitiveValue(value, sensitiveValues)
}

func containsUnredactableSensitiveJSONObject(raw []byte, sensitiveValues []string) bool {
	fields, ok := parseSensitiveJSONObject(raw)
	if !ok {
		return true
	}

	for _, field := range fields {
		if sensitiveJSONField(field.name) {
			continue
		}

		if containsSensitiveBodyMarker([]byte(field.name)) ||
			(bytes.Contains(
				raw,
				[]byte(`\u`),
			) && containsSensitiveValue(field.name, sensitiveValues)) {
			return true
		}

		if containsUnredactableSensitiveJSON(field.value, sensitiveValues) {
			return true
		}
	}

	return false
}

func containsUnredactableSensitiveJSONArray(raw []byte, sensitiveValues []string) bool {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return true
	}

	for _, item := range items {
		if containsUnredactableSensitiveJSON(item, sensitiveValues) {
			return true
		}
	}

	return false
}

func containsSensitiveJSONScalar(raw []byte, sensitiveValues []string) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if containsSensitiveJSONValue(decoder, sensitiveValues) {
		return true
	}

	return decoder.More()
}

func containsSensitiveJSONValue(decoder *json.Decoder, sensitiveValues []string) bool {
	token, err := decoder.Token()
	if err != nil {
		return true
	}

	if value, ok := token.(string); ok {
		return containsSensitiveBodyMarker([]byte(value)) ||
			containsSensitiveValue(value, sensitiveValues)
	}

	if number, ok := token.(json.Number); ok {
		return containsSensitiveValue(number.String(), sensitiveValues)
	}

	delimiter, ok := token.(json.Delim)
	if !ok {
		return false
	}

	switch delimiter {
	case '{':
		return containsSensitiveJSONObject(decoder, sensitiveValues)
	case '[':
		return containsSensitiveJSONArray(decoder, sensitiveValues)
	default:
		return true
	}
}

func containsSensitiveJSONObject(decoder *json.Decoder, sensitiveValues []string) bool {
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return true
		}

		name, ok := key.(string)
		if !ok ||
			containsSensitiveBodyMarker([]byte(name)) ||
			containsSensitiveValue(name, sensitiveValues) {
			return true
		}

		if containsSensitiveJSONValue(decoder, sensitiveValues) {
			return true
		}
	}

	return consumesJSONClosingDelimiter(decoder, '}')
}

func containsSensitiveJSONArray(decoder *json.Decoder, sensitiveValues []string) bool {
	for decoder.More() {
		if containsSensitiveJSONValue(decoder, sensitiveValues) {
			return true
		}
	}

	return consumesJSONClosingDelimiter(decoder, ']')
}

func containsSensitiveValue(value string, sensitiveValues []string) bool {
	for _, sensitiveValue := range sensitiveValues {
		if sensitiveValue != "" && strings.Contains(value, sensitiveValue) {
			return true
		}
	}

	return false
}
