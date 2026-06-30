package adapter

import (
	"encoding/json"
	"strings"
)

// responseFormatType enumerates the OpenAI `response_format.type`
// values the adapter parses.
type responseFormatType string

const (
	responseFormatJSONObject responseFormatType = "json_object"
	responseFormatJSONSchema responseFormatType = "json_schema"
)

// JSONResponseSpec describes what the caller asked for in
// `response_format`. The adapter uses this to coerce claude into
// returning parseable JSON since claude does not natively honor
// OpenAI's structured-output contract.
type JSONResponseSpec struct {
	// Mode is one of "json_object" or "json_schema". An empty Mode
	// means the caller did not request structured output.
	Mode string
	// SchemaName is the optional name from json_schema entries.
	SchemaName string
	// Schema is the JSON Schema body when Mode == "json_schema".
	Schema json.RawMessage
}

// ParseResponseFormat extracts a JSONResponseSpec from the raw
// `response_format` field in a ChatRequest. Returns an empty
// JSONResponseSpec when the field is missing or names plain text.
func ParseResponseFormat(raw json.RawMessage) JSONResponseSpec {
	if len(raw) == 0 {
		return JSONResponseSpec{Mode: "", SchemaName: "", Schema: nil}
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil {
		return JSONResponseSpec{Mode: "", SchemaName: "", Schema: nil}
	}
	switch responseFormatType(rf.Type) {
	case responseFormatJSONObject:
		return JSONResponseSpec{Mode: "json_object", SchemaName: "", Schema: nil}
	case responseFormatJSONSchema:
		if rf.JSONSchema == nil {
			return JSONResponseSpec{Mode: "json_schema", SchemaName: "", Schema: nil}
		}
		return JSONResponseSpec{
			Mode:       "json_schema",
			SchemaName: rf.JSONSchema.Name,
			Schema:     rf.JSONSchema.Schema,
		}
	}
	return JSONResponseSpec{
		Mode: "",

		// SystemPrompt returns text to inject into claude's system prompt so
		// the model emits raw JSON. The retryHint flag adds extra emphasis
		// for the second attempt after a failed parse.
		SchemaName: "", Schema: nil,
	}
}

// SystemPrompt is part of Clyde's typed adapter surface.
func (s JSONResponseSpec) SystemPrompt(retryHint bool) string {
	if s.Mode == "" {
		return ""
	}
	var b strings.Builder
	if retryHint {
		b.WriteString("Your previous response failed JSON.parse. ")
	}
	b.WriteString("You must respond with ONLY raw JSON. ")
	b.WriteString("No prose. No markdown code fences. No commentary before or after. ")
	b.WriteString("The first character of your reply must be `{` or `[`. ")
	if s.Mode == "json_schema" && len(s.Schema) > 0 {
		b.WriteString("The JSON must conform to this JSON Schema:\n")
		b.Write(s.Schema)
		b.WriteString("\n")
		if s.SchemaName != "" {
			b.WriteString("(schema name: ")
			b.WriteString(s.SchemaName)
			b.WriteString(")\n")
		}
	}
	return b.String()
}

// CoerceJSON returns text stripped of common LLM packaging that
// breaks JSON.parse: leading prose, trailing prose, surrounding
// markdown code fences, and a few stubborn single-quote / smart-
// quote slips. The function never alters the JSON shape; it only
// trims and unwraps. Callers should still [json.Unmarshal] the result
// to confirm validity.
func CoerceJSON(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return s
	}
	// Strip a single ```json ... ``` or ``` ... ``` fence.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```JSON")
		s = strings.TrimPrefix(s, "```")
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	// Trim any text before the first JSON delimiter.
	if i := indexOfFirstJSONDelim(s); i > 0 {
		s = s[i:]
	}
	// Trim any trailing text after the matching closing delimiter.
	if end := indexOfLastJSONDelim(s); end >= 0 && end+1 < len(s) {
		s = s[:end+1]
	}
	return s
}

func indexOfFirstJSONDelim(s string) int {
	for i, r := range s {
		if r == '{' || r == '[' {
			return i
		}
	}
	return -1
}

func indexOfLastJSONDelim(s string) int {
	last := -1
	for i, r := range s {
		if r == '}' || r == ']' {
			last = i
		}
	}
	return last
}

// LooksLikeJSON returns true when text parses as a JSON value.
func LooksLikeJSON(text string) bool {
	if text == "" {
		return false
	}
	var v json.RawMessage
	return json.Unmarshal([]byte(text), &v) == nil
}
