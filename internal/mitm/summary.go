package mitm

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

type captureBodyIndex struct {
	raw json.RawMessage
}

func emptyCaptureBodyIndex() captureBodyIndex {
	return captureBodyIndex{raw: nil}
}

type captureBodySummary struct {
	Mode     string   `json:"mode,omitempty"`
	BodyType string   `json:"body_type,omitempty"`
	Bytes    int      `json:"bytes,omitempty"`
	SHA256   string   `json:"sha256,omitempty"`
	Keys     []string `json:"keys,omitempty"`
	Messages int      `json:"messages,omitempty"`
	Input    int      `json:"input,omitempty"`
	Tools    int      `json:"tools,omitempty"`
	Model    string   `json:"model,omitempty"`
	ArrayLen int      `json:"array_len,omitempty"`
}

func newCaptureBodyIndexFromSummary(summary captureBodySummary) captureBodyIndex {
	raw, err := json.Marshal(summary)
	if err != nil {
		raw = json.RawMessage(`null`)
	}

	return captureBodyIndex{raw: raw}
}

func bodyTypeForCapture(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "empty"
	}

	switch trimmed[0] {
	case '{':
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return "json_object"
		}
	case '[':
		var decoded []json.RawMessage
		if err := json.Unmarshal(trimmed, &decoded); err == nil {
			return "json_array"
		}
	}

	var scalar json.RawMessage
	if err := json.Unmarshal(trimmed, &scalar); err != nil {
		return "bytes"
	}

	return "json_scalar"
}

func bodyKeysForCapture(body []byte) []string {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &decoded); err != nil {
		return nil
	}

	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	return keys
}

func summarizeBody(body []byte) captureBodySummary {
	summary := captureBodySummary{
		Mode:     "filtered_inline",
		BodyType: bodyTypeForCapture(body),
		Bytes:    len(body),
		SHA256:   sha256Hex(body),
		Keys:     nil,
		Messages: 0,
		Input:    0,
		Tools:    0,
		Model:    "",
		ArrayLen: 0,
	}

	return summarizeJSON(body, summary)
}

func summarizeJSON(body []byte, summary captureBodySummary) captureBodySummary {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return summary
	}

	if summary.BodyType == "bytes" {
		// Non-JSON bodies carry no field set to summarize, and the raw bytes are
		// never inlined into a summary; the full body lives only in the SQLite
		// capture store.
		return summary
	}

	if summary.BodyType == "json_array" {
		var values []json.RawMessage
		if err := json.Unmarshal(body, &values); err == nil {
			summary.ArrayLen = len(values)
		}

		return summary
	}

	if summary.BodyType != "json_object" {
		return summary
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return summary
	}

	summary.Keys = bodyKeysForCapture(body)
	summary.Messages = rawFieldArrayLen(fields, "messages")
	summary.Input = rawFieldArrayLen(fields, "input")
	summary.Tools = rawFieldArrayLen(fields, "tools")
	summary.Model = rawFieldString(fields, "model")
	if features, err := ExtractRequestFeatures(CapturedRequest{
		RequestHeaders: nil,
		RequestBody:    json.RawMessage(body),
	}); err == nil {
		summary.Model = features.ModelID
	}

	return summary
}

func rawFieldArrayLen(fields map[string]json.RawMessage, key string) int {
	raw, ok := fields[key]
	if !ok {
		return 0
	}

	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return 0
	}

	return len(values)
}

func rawFieldString(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}

	return value
}
