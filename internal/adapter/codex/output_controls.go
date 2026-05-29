package codex

import (
	"encoding/json"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// OutputControls carries the codex-cli output-shaping fields Clyde
// forwards on the Responses request. codex-cli sends `text` (verbosity
// and JSON-schema output formatting); it does not send a
// max_completion_tokens, truncation, or prompt_cache_retention field, so
// those are not modeled here. Output capping relies on the upstream
// server-side policy, exactly as codex-cli relies on it.
type OutputControls struct {
	Text json.RawMessage `json:"text,omitempty"`
}

// BuildOutputControls extracts the codex-cli-faithful output controls
// from an inbound ChatRequest. Only `text` survives; the inbound
// max-token, truncation, and prompt-cache-retention hints are dropped
// because codex-cli never sends those wire fields.
func BuildOutputControls(req adapteropenai.ChatRequest) OutputControls {
	return OutputControls{
		Text: validJSONObject(req.Text),
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
