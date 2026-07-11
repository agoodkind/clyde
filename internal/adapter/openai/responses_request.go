package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// ResponsesRequest is the typed subset of the OpenAI Responses API
// request the adapter consumes. Fields the adapter projects into the
// shared chat resolve pipeline are typed; the genuinely open-ended
// Responses payloads (input, tools schema passthrough, structured text
// config, metadata) stay as [json.RawMessage] isolated at this edge with
// a comment, because their full shape is an external contract the
// adapter forwards rather than models.
type ResponsesRequest struct {
	Model                string          `json:"model"`
	Instructions         string          `json:"instructions,omitempty"`
	Input                json.RawMessage `json:"input,omitempty"`
	Stream               bool            `json:"stream,omitempty"`
	Tools                []Tool          `json:"tools,omitempty"`
	ToolChoice           json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning            *Reasoning      `json:"reasoning,omitempty"`
	MaxOutputTokens      *int            `json:"max_output_tokens,omitempty"`
	Temperature          *float64        `json:"temperature,omitempty"`
	TopP                 *float64        `json:"top_p,omitempty"`
	Stop                 json.RawMessage `json:"stop,omitempty"`
	ParallelTools        *bool           `json:"parallel_tool_calls,omitempty"`
	Store                *bool           `json:"store,omitempty"`
	Metadata             json.RawMessage `json:"metadata,omitempty"`
	Include              []string        `json:"include,omitempty"`
	ServiceTier          string          `json:"service_tier,omitempty"`
	Text                 json.RawMessage `json:"text,omitempty"`
	Truncation           string          `json:"truncation,omitempty"`
	PromptCacheRetention string          `json:"prompt_cache_retention,omitempty"`
}

// UnmarshalResponsesRequest decodes a POST /v1/responses body into the
// typed ResponsesRequest. It performs shape decoding only; capability
// validation and model resolution run in the shared adapter pipeline.
func UnmarshalResponsesRequest(body []byte) (ResponsesRequest, error) {
	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		slog.Warn("adapter.openai.responses_request_invalid", "concern", "adapter.chat.dispatch", "err", err)
		return ResponsesRequest{}, fmt.Errorf("unmarshal responses request: %w", err)
	}
	return req, nil
}
