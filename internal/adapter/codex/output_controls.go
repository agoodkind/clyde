package codex

import (
	"encoding/json"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// OutputControls carries the Responses output-shaping fields Clyde
// forwards to Codex. OpenAI-compatible max_tokens is translated to the
// Responses max_output_tokens field. The chat-only max_completion_tokens,
// truncation, and prompt_cache_retention fields are not forwarded.
type OutputControls struct {
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Text            json.RawMessage `json:"text,omitempty"`
}

// BuildOutputControls extracts Responses-compatible output controls from
// an inbound ChatRequest.
func BuildOutputControls(req adapteropenai.ChatRequest) OutputControls {
	maxOutputTokens := req.MaxOutputTokens
	if maxOutputTokens == nil {
		maxOutputTokens = req.MaxTokens
	}
	return OutputControls{
		MaxOutputTokens: maxOutputTokens,
		Text:            validJSONObject(req.Text),
	}
}

func validJSONObject(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if !json.Valid(raw) {
		return nil
	}
	if trimmed[0] != '{' {
		return nil
	}
	return raw
}
