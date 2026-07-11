package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// ResponsesFieldPresence records a top-level field's decoded wire presence.
type ResponsesFieldPresence int

const (
	// ResponsesFieldAbsent means the key was not sent.
	ResponsesFieldAbsent ResponsesFieldPresence = iota
	// ResponsesFieldNull means the key was explicitly null.
	ResponsesFieldNull
	// ResponsesFieldEmpty means the key was an empty string, array, or object.
	ResponsesFieldEmpty
	// ResponsesFieldFalse means the key was explicitly false.
	ResponsesFieldFalse
	// ResponsesFieldZero means the key was explicitly numeric zero.
	ResponsesFieldZero
	// ResponsesFieldPresent means the key had any other JSON value.
	ResponsesFieldPresent
)

// ResponsesFieldSet records each top-level CreateResponse field's presence.
// It is populated while decoding, so later compatibility work does not need
// to parse the untrusted request body again.
type ResponsesFieldSet struct{ fields responsesRawFields }

// Presence returns the exact wire-presence classification for a known field.
func (s ResponsesFieldSet) Presence(name string) ResponsesFieldPresence {
	return responsesPresence(s.fields[name])
}

// responsesRawFields is the one deliberately opaque edge used solely to
// retain CreateResponse top-level presence during typed JSON decoding.
type responsesRawFields map[string]json.RawMessage

type responsesPresenceToken string

const (
	responsesPresenceTokenAbsent      responsesPresenceToken = ""
	responsesPresenceTokenNull        responsesPresenceToken = "null"
	responsesPresenceTokenEmptyString responsesPresenceToken = `""`
	responsesPresenceTokenEmptyArray  responsesPresenceToken = "[]"
	responsesPresenceTokenEmptyObject responsesPresenceToken = "{}"
	responsesPresenceTokenFalse       responsesPresenceToken = "false"
	responsesPresenceTokenZero        responsesPresenceToken = "0"
)

// ResponsesRequest is the typed subset of the OpenAI Responses API
// request the adapter consumes. Fields the adapter projects into the
// shared chat resolve pipeline are typed; the genuinely open-ended
// Responses payloads (input, tools schema passthrough, structured text
// config, metadata) stay as [json.RawMessage] isolated at this edge with
// a comment, because their full shape is an external contract the
// adapter forwards rather than models.
type ResponsesRequest struct {
	PreviousResponseID   *string           `json:"previous_response_id,omitempty"`
	Model                string            `json:"model"`
	Background           *bool             `json:"background,omitempty"`
	MaxToolCalls         *int              `json:"max_tool_calls,omitempty"`
	Text                 json.RawMessage   `json:"text,omitempty"`
	Tools                json.RawMessage   `json:"tools,omitempty"`
	ToolChoice           json.RawMessage   `json:"tool_choice,omitempty"`
	Prompt               json.RawMessage   `json:"prompt,omitempty"`
	PromptCacheOptions   json.RawMessage   `json:"prompt_cache_options,omitempty"`
	TopLogprobs          *int              `json:"top_logprobs,omitempty"`
	Metadata             json.RawMessage   `json:"metadata,omitempty"`
	Temperature          *float64          `json:"temperature,omitempty"`
	TopP                 *float64          `json:"top_p,omitempty"`
	User                 *string           `json:"user,omitempty"`
	SafetyIdentifier     *string           `json:"safety_identifier,omitempty"`
	PromptCacheKey       *string           `json:"prompt_cache_key,omitempty"`
	ServiceTier          *string           `json:"service_tier,omitempty"`
	PromptCacheRetention *string           `json:"prompt_cache_retention,omitempty"`
	Truncation           *string           `json:"truncation,omitempty"`
	Reasoning            *Reasoning        `json:"reasoning,omitempty"`
	Input                json.RawMessage   `json:"input,omitempty"`
	Include              []string          `json:"include,omitempty"`
	ParallelTools        *bool             `json:"parallel_tool_calls,omitempty"`
	Store                *bool             `json:"store,omitempty"`
	Instructions         *string           `json:"instructions,omitempty"`
	Moderation           json.RawMessage   `json:"moderation,omitempty"`
	Stream               *bool             `json:"stream,omitempty"`
	StreamOptions        json.RawMessage   `json:"stream_options,omitempty"`
	Conversation         json.RawMessage   `json:"conversation,omitempty"`
	ContextManagement    json.RawMessage   `json:"context_management,omitempty"`
	MaxOutputTokens      *int              `json:"max_output_tokens,omitempty"`
	MaxTokens            *int              `json:"max_tokens,omitempty"`
	MaxCompletionTokens  *int              `json:"max_completion_tokens,omitempty"`
	N                    *int              `json:"n,omitempty"`
	Stop                 json.RawMessage   `json:"stop,omitempty"`
	Fields               ResponsesFieldSet `json:"-"`
	// Tools stays raw at this edge because the Responses tools array mixes
	// client-owned function tools with OpenAI built-ins (web_search,
	// file_search, computer_use, mcp) and public custom tools whose full
	// shape is an external contract. The strict per-element Tool unmarshal
	// rejects built-ins, so the projection splits this raw array leniently
	// with SplitResponsesTools instead of decoding it into typed Tools here.
}

// UnmarshalJSON decodes the request and records top-level wire presence.
func (r *ResponsesRequest) UnmarshalJSON(data []byte) error {
	type wire ResponsesRequest
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode typed Responses request: %w", err)
	}
	fields := responsesRawFields{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode Responses field presence: %w", err)
	}
	*r = ResponsesRequest(decoded)
	r.Fields = ResponsesFieldSet{fields: fields}
	return nil
}

func responsesPresence(raw json.RawMessage) ResponsesFieldPresence {
	value := responsesPresenceToken(strings.TrimSpace(string(raw)))
	switch value {
	case responsesPresenceTokenAbsent:
		return ResponsesFieldAbsent
	case responsesPresenceTokenNull:
		return ResponsesFieldNull
	case responsesPresenceTokenEmptyString, responsesPresenceTokenEmptyArray, responsesPresenceTokenEmptyObject:
		return ResponsesFieldEmpty
	case responsesPresenceTokenFalse:
		return ResponsesFieldFalse
	case responsesPresenceTokenZero:
		return ResponsesFieldZero
	default:
		return ResponsesFieldPresent
	}
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
