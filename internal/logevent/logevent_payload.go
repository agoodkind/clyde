package logevent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

// PayloadView is the inline payload representation logged with each
// request story. Large context fields are summarized into Removed
// instead of stored inline.
type PayloadView struct {
	Summary PayloadSummary   `json:"payload_summary,omitzero"`
	Fields  []PayloadField   `json:"payload_fields,omitempty"`
	Removed []PayloadRemoved `json:"payload_removed,omitempty"`
}

// PayloadSummary records size and shape without storing raw context payloads.
type PayloadSummary struct {
	ContentType string `json:"content_type,omitempty"`
	BodyType    string `json:"body_type,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	FieldCount  int    `json:"field_count,omitempty"`
	ArrayItems  int    `json:"array_items,omitempty"`
}

// PayloadField stores a retained JSON field from the fixed inline payload view.
type PayloadField struct {
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
	Bytes int             `json:"bytes"`
}

// PayloadRemoved records a context field removed from the inline payload view.
type PayloadRemoved struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
	Bytes  int    `json:"bytes"`
	Items  int    `json:"items,omitempty"`
}

func appendPayloadAttrs(attrs []slog.Attr, payload PayloadView) []slog.Attr {
	attrs = append(attrs, slog.Any("payload_summary", payload.Summary))
	if len(payload.Fields) > 0 {
		attrs = append(attrs, slog.Any("payload_fields", payload.Fields))
	}
	if len(payload.Removed) > 0 {
		attrs = append(attrs, slog.Any("payload_removed", payload.Removed))
	}
	return attrs
}

// FilterPayload returns the fixed inline payload view for normal JSONL logs.
func FilterPayload(raw []byte, contentType string) PayloadView {
	trimmed := strings.TrimSpace(string(raw))
	summary := PayloadSummary{
		ContentType: strings.TrimSpace(contentType),
		BodyType:    classifyJSONBytes([]byte(trimmed)),
		SHA256:      sha256Hex(raw),
		Bytes:       len(raw),
		FieldCount:  0,
		ArrayItems:  0,
	}
	view := PayloadView{Summary: summary, Fields: nil, Removed: nil}
	if trimmed == "" {
		view.Summary.BodyType = "empty"
		return view
	}
	if strings.HasPrefix(trimmed, "{") {
		return filterJSONObject([]byte(trimmed), view)
	}
	if strings.HasPrefix(trimmed, "[") {
		var values []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &values); err != nil {
			view.Summary.BodyType = "bytes"
			return view
		}
		view.Summary.BodyType = "json_array"
		view.Summary.ArrayItems = len(values)
		view.Removed = append(view.Removed, PayloadRemoved{Path: "$", Reason: "array_context", Bytes: len(trimmed), Items: len(values)})
		return view
	}
	return view
}

func filterJSONObject(raw []byte, view PayloadView) PayloadView {
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil {
		view.Summary.BodyType = "bytes"
		return view
	}
	view.Summary.BodyType = "json_object"
	view.Summary.FieldCount = len(fields)
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := fields[key]
		path := "$" + "." + key
		filteredValue, removed, keep := filterPayloadField(value, path, key)
		view.Removed = append(view.Removed, removed...)
		if !keep {
			continue
		}
		view.Fields = append(view.Fields, PayloadField{Path: path, Value: filteredValue, Bytes: len(filteredValue)})
	}
	return view
}

func filterPayloadField(raw json.RawMessage, path string, key string) (json.RawMessage, []PayloadRemoved, bool) {
	if isContextField(key) {
		return nil, []PayloadRemoved{{
			Path:   path,
			Reason: "large_context",
			Bytes:  len(raw),
			Items:  countJSONItems(raw),
		}}, false
	}
	filteredValue, removed := filterPayloadValue(raw, path)
	return filteredValue, removed, true
}

func filterPayloadValue(raw json.RawMessage, path string) (json.RawMessage, []PayloadRemoved) {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		return filterPayloadObjectValue(raw, path)
	}
	if strings.HasPrefix(trimmed, "[") {
		return filterPayloadArrayValue(raw, path)
	}
	return cloneRaw(raw), nil
}

func filterPayloadObjectValue(raw json.RawMessage, path string) (json.RawMessage, []PayloadRemoved) {
	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return cloneRaw(raw), nil
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	filteredFields := make(map[string]json.RawMessage, len(fields))
	removedFields := make([]PayloadRemoved, 0)
	for _, key := range keys {
		fieldPath := path + "." + key
		filteredValue, removed, keep := filterPayloadField(fields[key], fieldPath, key)
		removedFields = append(removedFields, removed...)
		if keep {
			filteredFields[key] = filteredValue
		}
	}
	filteredRaw, err := json.Marshal(filteredFields)
	if err != nil {
		return cloneRaw(raw), removedFields
	}
	return filteredRaw, removedFields
}

func filterPayloadArrayValue(raw json.RawMessage, path string) (json.RawMessage, []PayloadRemoved) {
	values := make([]json.RawMessage, 0)
	if err := json.Unmarshal(raw, &values); err != nil {
		return cloneRaw(raw), nil
	}
	filteredValues := make([]json.RawMessage, 0, len(values))
	removedFields := make([]PayloadRemoved, 0)
	for i, value := range values {
		itemPath := path + "[" + strconv.Itoa(i) + "]"
		filteredValue, removed := filterPayloadValue(value, itemPath)
		filteredValues = append(filteredValues, filteredValue)
		removedFields = append(removedFields, removed...)
	}
	filteredRaw, err := json.Marshal(filteredValues)
	if err != nil {
		return cloneRaw(raw), removedFields
	}
	return filteredRaw, removedFields
}

type contextFieldKey string

const (
	contextFieldMessages     contextFieldKey = "messages"
	contextFieldMessage      contextFieldKey = "message"
	contextFieldInput        contextFieldKey = "input"
	contextFieldInputs       contextFieldKey = "inputs"
	contextFieldTools        contextFieldKey = "tools"
	contextFieldTool         contextFieldKey = "tool"
	contextFieldFunctions    contextFieldKey = "functions"
	contextFieldFunction     contextFieldKey = "function"
	contextFieldInstructions contextFieldKey = "instructions"
	contextFieldPrompt       contextFieldKey = "prompt"
	contextFieldPrompts      contextFieldKey = "prompts"
	contextFieldConversation contextFieldKey = "conversation"
	contextFieldChats        contextFieldKey = "chats"
	contextFieldContext      contextFieldKey = "context"
	contextFieldSystem       contextFieldKey = "system"
)

func isContextField(key string) bool {
	normalized := contextFieldKey(strings.ToLower(strings.TrimSpace(key)))
	switch normalized {
	case contextFieldMessages,
		contextFieldMessage,
		contextFieldInput,
		contextFieldInputs,
		contextFieldTools,
		contextFieldTool,
		contextFieldFunctions,
		contextFieldFunction,
		contextFieldInstructions,
		contextFieldPrompt,
		contextFieldPrompts,
		contextFieldConversation,
		contextFieldChats,
		contextFieldContext,
		contextFieldSystem:
		return true
	default:
		return false
	}
}

func countJSONItems(raw json.RawMessage) int {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err == nil {
			return len(values)
		}
	}
	if strings.HasPrefix(trimmed, "{") {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err == nil {
			return len(values)
		}
	}
	if trimmed == "" {
		return 0
	}
	return 1
}

func classifyJSONBytes(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		return "empty"
	case strings.HasPrefix(trimmed, "{"):
		return "json_object"
	case strings.HasPrefix(trimmed, "["):
		return "json_array"
	case strings.HasPrefix(trimmed, `"`), trimmed == "true", trimmed == "false", trimmed == "null":
		return "json_scalar"
	default:
		return "bytes"
	}
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	cloned := make(json.RawMessage, len(raw))
	copy(cloned, raw)
	return cloned
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
