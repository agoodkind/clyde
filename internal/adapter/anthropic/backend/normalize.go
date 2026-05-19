package anthropicbackend

import (
	"encoding/json"
	"strings"
)

// contentKind discriminates NormalizeContent wire shapes.
type contentKind int

const (
	contentKindEmpty contentKind = iota
	contentKindString
	contentKindParts
)

// normalizeContent parses OpenAIMessage.Content into typed parts (mirrors adapter.NormalizeContent).
func normalizeContent(raw json.RawMessage) ([]OpenAIContentPart, contentKind) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, contentKindEmpty
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []OpenAIContentPart{textContentPart(s)}, contentKindString
	}
	var parts []OpenAIContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		for i := range parts {
			if parts[i].Type == "" {
				parts[i].Type = "text"
			}
		}
		return parts, contentKindParts
	}
	return []OpenAIContentPart{textContentPart(string(raw))}, contentKindString
}

func textContentPart(text string) OpenAIContentPart {
	return OpenAIContentPart{
		Type:      "text",
		Text:      text,
		Thinking:  "",
		Signature: "",
		ImageURL:  nil,
		Audio:     nil,
		Refusal:   "",
		ToolUseID: "",
		Content:   nil,
		ID:        "",
		Name:      "",
		Input:     nil,
	}
}

// flattenContent collapses message content to a single string for system collection.
func flattenContent(raw json.RawMessage) string {
	parts, kind := normalizeContent(raw)
	if kind == contentKindString {
		if len(parts) == 0 {
			return ""
		}
		return parts[0].Text
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "image_url":
			b.WriteString("[image]")
		case "input_audio":
			b.WriteString("[audio]")
		case "refusal":
			b.WriteString("[refusal: ")
			b.WriteString(p.Refusal)
			b.WriteString("]")
		default:
			b.WriteString("[")
			b.WriteString(p.Type)
			b.WriteString("]")
		}
	}
	return b.String()
}
