package codex

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/codexwire"
)

// responseCreateWsEnvelope is the marshal envelope for
// ResponseCreateWsRequest. The standard json:omitempty rules apply
// for every field; the empty-array overrides for input and tools
// are applied after by patchEmptyArrayFields.
type responseCreateWsEnvelope struct {
	Type               string                       `json:"type,omitempty"`
	Model              string                       `json:"model,omitempty"`
	Instructions       string                       `json:"instructions,omitempty"`
	Input              []codexwire.InputItem        `json:"input,omitempty"`
	Tools              []codexwire.ToolSpec         `json:"tools,omitempty"`
	ToolChoice         string                       `json:"tool_choice,omitempty"`
	ParallelToolCalls  bool                         `json:"parallel_tool_calls,omitempty"`
	Reasoning          *Reasoning                   `json:"reasoning,omitempty"`
	Store              bool                         `json:"store"`
	Stream             bool                         `json:"stream"`
	Include            []string                     `json:"include,omitempty"`
	ServiceTier        string                       `json:"service_tier,omitempty"`
	PromptCacheKey     string                       `json:"prompt_cache_key,omitempty"`
	MaxOutputTokens    *int                         `json:"max_output_tokens,omitempty"`
	Text               json.RawMessage              `json:"text,omitempty"`
	ClientMetadata     ResponseCreateClientMetadata `json:"client_metadata,omitempty"`
	PreviousResponseID string                       `json:"previous_response_id,omitempty"`
	Generate           *bool                        `json:"generate,omitempty"`
}

// MarshalResponseCreateWsRequest is part of Clyde's typed adapter
// surface. The marshalled frame must carry `input: []` whenever the
// request semantically carries an empty input but the field would
// otherwise be omitted by json:omitempty. The upstream rejects the
// frame with "Missing required parameter: 'input'" when the field
// is absent. Cases:
//   - Warmup (Generate == false): always sends empty input.
//   - Continuation (PreviousResponseID set, no new items): the
//     prior response's items are server-side; we send no delta.
func MarshalResponseCreateWsRequest(req ResponseCreateWsRequest) ([]byte, error) {
	isWarmup := req.Generate != nil && !*req.Generate
	forceEmptyInput := req.Input != nil && len(req.Input) == 0 &&
		(isWarmup || req.PreviousResponseID != "")
	forceEmptyTools := isWarmup && req.Tools != nil && len(req.Tools) == 0
	envelope := responseCreateWsEnvelope(req)
	raw, err := json.Marshal(envelope)
	if err != nil {
		slog.Warn("adapter.codex.ws.marshal_request_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("marshal websocket response.create request: %w", err)
	}
	if !forceEmptyInput && !forceEmptyTools {
		return raw, nil
	}
	return patchEmptyArrayFields(raw, forceEmptyInput, forceEmptyTools)
}

// patchEmptyArrayFields re-encodes raw with `input: []` and/or
// `tools: []` added so the frame satisfies the upstream's
// required-field rule for warmup and continuation frames.
func patchEmptyArrayFields(raw []byte, forceInput, forceTools bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Warn("adapter.codex.ws.unmarshal_request_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("unmarshal websocket response.create request: %w", err)
	}
	if forceInput {
		payload["input"] = json.RawMessage("[]")
	}
	if forceTools {
		payload["tools"] = json.RawMessage("[]")
	}
	rewritten, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("adapter.codex.ws.remarshal_request_failed", "concern", "adapter.providers.codex.request", "err", err)
		return nil, fmt.Errorf("marshal rewritten websocket response.create request: %w", err)
	}
	return rewritten, nil
}
