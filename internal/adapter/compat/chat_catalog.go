package compat

import (
	"encoding/json"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

// The generic OpenAI listener returns these codes in a 400 error for a
// request field that Clyde cannot honor.
const (
	// RejectionCodeUnsupportedParameter reports a documented field that the
	// resolved provider cannot honor with the requested value.
	RejectionCodeUnsupportedParameter = "unsupported_parameter"
	// RejectionCodeUnknownParameter reports a field that neither the
	// documented schema nor Clyde's extension fields define.
	RejectionCodeUnknownParameter = "unknown_parameter"
	// RejectionCodeInvalidParameter reports a field combination that the
	// documented schema forbids.
	RejectionCodeInvalidParameter = "invalid_parameter"
)

// A presence callback returns chatPresenceAbsent for a missing key,
// chatPresenceNull for an explicit null, and chatPresenceEmpty for an empty
// string, array, or object. Any other value means the request set the key.
const (
	chatPresenceAbsent = 0
	chatPresenceNull   = 1
	chatPresenceEmpty  = 2
)

// Rejection reports one rejected request field. Message never includes a
// request value.
type Rejection struct {
	Code    string
	Param   string
	Message string
}

// ChatRequestValues stores the decoded values that ChatRejection compares
// against provider limits.
type ChatRequestValues struct {
	Stream           bool
	Temperature      *float64
	TopP             *float64
	PresencePenalty  *float64
	FrequencyPenalty *float64
	N                *int
	Logprobs         *bool
	TopLogprobs      *int
	Store            *bool
	ServiceTier      string
	ToolChoice       json.RawMessage
	FunctionCall     json.RawMessage
	Modalities       json.RawMessage
	ResponseFormat   json.RawMessage
	UnknownKeys      []string
}

// The typed Chat request omits these documented fields. They tune caching,
// abuse monitoring, or latency and do not change generated output.
var chatAcceptedUnknownKeys = map[string]bool{
	"prompt_cache_key":     true,
	"prompt_cache_options": true,
	"safety_identifier":    true,
	"prediction":           true,
}

// The typed Chat request omits these documented fields. They change
// generated output, and no provider supports them.
var chatUnsupportedDocumentedKeys = map[string]bool{
	"verbosity":          true,
	"web_search_options": true,
	"moderation":         true,
}

// ChatRejection returns the first request field that the resolved provider
// cannot honor as the Chat Completions reference documents it. presenceFor
// returns the presence of a top-level key. ChatRejection checks unknown keys
// and the stream_options rule for every provider. The OpenAI-compatible
// passthrough has no catalog column and skips the provider checks.
func ChatRejection(presenceFor func(string) int, values ChatRequestValues, provider adaptermodel.BackendID) (Rejection, bool) {
	if rejection, ok := chatUnknownKeyRejection(values.UnknownKeys); ok {
		return rejection, true
	}
	if presenceSet(presenceFor("stream_options")) && !values.Stream {
		return Rejection{
			Code:    RejectionCodeInvalidParameter,
			Param:   "stream_options",
			Message: "The 'stream_options' parameter is only allowed when 'stream' is enabled.",
		}, true
	}
	column, known := providerColumnFor(provider)
	if !known {
		return noRejection(), false
	}
	for _, check := range chatFieldChecks(presenceFor, values, column) {
		if check.unsupported() {
			return unsupportedParameter(check.param, column), true
		}
	}
	return noRejection(), false
}

type chatFieldCheck struct {
	param       string
	unsupported func() bool
}

// Codex ignores sampling controls, output caps, stop sequences, forced tool
// choice, and structured output formats. Anthropic clamps temperature above
// 1 and offers no processing tier. Neither provider reads n, penalties,
// logit bias, logprobs, seed, audio, non-text modalities, stored
// completions, or the legacy function_call control.
func chatFieldChecks(presenceFor func(string) int, values ChatRequestValues, column providerColumn) []chatFieldCheck {
	codex := column == columnCodex
	return []chatFieldCheck{
		{param: "n", unsupported: func() bool { return values.N != nil && *values.N > 1 }},
		{param: "temperature", unsupported: func() bool { return temperatureUnsupported(column, values.Temperature) }},
		{param: "top_p", unsupported: func() bool { return codex && numberDiffers(values.TopP, 1) }},
		{param: "max_tokens", unsupported: func() bool { return codex && presenceSet(presenceFor("max_tokens")) }},
		{param: "max_completion_tokens", unsupported: func() bool { return codex && presenceSet(presenceFor("max_completion_tokens")) }},
		{param: "max_output_tokens", unsupported: func() bool { return codex && presenceSet(presenceFor("max_output_tokens")) }},
		{param: "stop", unsupported: func() bool { return codex && valueSet(presenceFor("stop")) }},
		{param: "presence_penalty", unsupported: func() bool { return numberDiffers(values.PresencePenalty, 0) }},
		{param: "frequency_penalty", unsupported: func() bool { return numberDiffers(values.FrequencyPenalty, 0) }},
		{param: "logit_bias", unsupported: func() bool { return valueSet(presenceFor("logit_bias")) }},
		{param: "logprobs", unsupported: func() bool { return values.Logprobs != nil && *values.Logprobs }},
		{param: "top_logprobs", unsupported: func() bool { return values.TopLogprobs != nil && *values.TopLogprobs > 0 }},
		{param: "seed", unsupported: func() bool { return presenceSet(presenceFor("seed")) }},
		{param: "audio", unsupported: func() bool { return presenceSet(presenceFor("audio")) }},
		{param: "modalities", unsupported: func() bool { return modalitiesUnsupported(values.Modalities) }},
		{param: "store", unsupported: func() bool { return values.Store != nil && *values.Store }},
		{param: "service_tier", unsupported: func() bool { return !codex && serviceTierUnsupported(values.ServiceTier) }},
		{param: "tool_choice", unsupported: func() bool { return codex && !choiceIsAuto(values.ToolChoice) }},
		{param: "function_call", unsupported: func() bool { return !choiceIsAuto(values.FunctionCall) }},
		{param: "response_format", unsupported: func() bool { return codex && !responseFormatIsText(values.ResponseFormat) }},
	}
}

func noRejection() Rejection {
	return Rejection{Code: "", Param: "", Message: ""}
}

func chatUnknownKeyRejection(unknownKeys []string) (Rejection, bool) {
	for _, key := range unknownKeys {
		if chatAcceptedUnknownKeys[key] {
			continue
		}
		if chatUnsupportedDocumentedKeys[key] {
			return Rejection{
				Code:    RejectionCodeUnsupportedParameter,
				Param:   key,
				Message: "Unsupported parameter: '" + key + "' is not supported by Clyde.",
			}, true
		}
		return Rejection{
			Code:    RejectionCodeUnknownParameter,
			Param:   key,
			Message: "Unrecognized request argument supplied: " + key,
		}, true
	}
	return noRejection(), false
}

func unsupportedParameter(param string, column providerColumn) Rejection {
	return Rejection{
		Code:    RejectionCodeUnsupportedParameter,
		Param:   param,
		Message: "Unsupported parameter: '" + param + "' is not supported with the " + backendLabel(column) + " backend.",
	}
}

func presenceSet(presence int) bool {
	return presence != chatPresenceAbsent && presence != chatPresenceNull
}

func valueSet(presence int) bool {
	return presenceSet(presence) && presence != chatPresenceEmpty
}

func numberDiffers(value *float64, documentedDefault float64) bool {
	return value != nil && *value != documentedDefault
}

// Codex ignores temperature, and this check accepts only the default of 1.
// Anthropic clamps temperature above 1, and this check rejects those values.
func temperatureUnsupported(column providerColumn, temperature *float64) bool {
	if temperature == nil {
		return false
	}
	if column == columnCodex {
		return *temperature != 1
	}
	return *temperature > 1
}

func modalitiesUnsupported(raw json.RawMessage) bool {
	if isNullOrEmptyJSON(raw) {
		return false
	}
	var modalities []string
	if err := json.Unmarshal(raw, &modalities); err != nil {
		return true
	}
	for _, modality := range modalities {
		if modality != "text" {
			return true
		}
	}
	return false
}

var defaultServiceTiers = map[string]bool{
	"":        true,
	"auto":    true,
	"default": true,
}

func serviceTierUnsupported(tier string) bool {
	return !defaultServiceTiers[strings.TrimSpace(tier)]
}

func choiceIsAuto(raw json.RawMessage) bool {
	if isNullOrEmptyJSON(raw) {
		return true
	}
	var choice string
	if err := json.Unmarshal(raw, &choice); err != nil {
		return false
	}
	return choice == "auto"
}

func responseFormatIsText(raw json.RawMessage) bool {
	if isNullOrEmptyJSON(raw) {
		return true
	}
	var format struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &format); err != nil {
		return false
	}
	return format.Type == "text"
}

func isNullOrEmptyJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}
