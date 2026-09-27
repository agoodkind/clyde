package compat

import (
	"encoding/json"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

// Rejection codes a documented-contract listener returns for a request
// field that Clyde cannot honor.
const (
	// RejectionCodeUnsupportedParameter marks a documented field the
	// resolved provider cannot honor with the requested value.
	RejectionCodeUnsupportedParameter = "unsupported_parameter"
	// RejectionCodeUnknownParameter marks a field outside the documented
	// request schema and outside Clyde's extension fields.
	RejectionCodeUnknownParameter = "unknown_parameter"
	// RejectionCodeInvalidParameter marks a field combination the
	// documented schema forbids.
	RejectionCodeInvalidParameter = "invalid_parameter"
)

// chatPresenceAbsent, chatPresenceNull, and chatPresenceEmpty are the
// presence values a presence callback returns for a missing key, an
// explicit null, and an empty string, array, or object. Every other value
// means the request set the field.
const (
	chatPresenceAbsent = 0
	chatPresenceNull   = 1
	chatPresenceEmpty  = 2
)

// Rejection is one field-level request rejection. Message is a sanitized
// sentence that never includes a request value.
type Rejection struct {
	Code    string
	Param   string
	Message string
}

// ChatRequestValues are the decoded Chat Completions values that decide a
// rejection by value instead of by presence alone.
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

// chatAcceptedUnknownKeys are documented Chat Completions fields that the
// adapter's typed request does not model and that do not change the
// generated output. They tune caching, abuse monitoring, or latency.
var chatAcceptedUnknownKeys = map[string]bool{
	"prompt_cache_key":     true,
	"prompt_cache_options": true,
	"safety_identifier":    true,
	"prediction":           true,
}

// chatUnsupportedDocumentedKeys are documented Chat Completions fields
// that the typed request does not model and that change the output. The
// adapter has no provider path for them.
var chatUnsupportedDocumentedKeys = map[string]bool{
	"verbosity":          true,
	"web_search_options": true,
	"moderation":         true,
}

// ChatRejection returns the first request field that the resolved
// provider cannot honor as the Chat Completions contract documents it.
// presenceFor reports a top-level key's presence. A provider without a
// catalog column, such as the OpenAI-compatible passthrough, receives the
// request unchanged and never produces a provider rejection.
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

// chatFieldCheck pairs a request field with the predicate that reports
// whether the resolved provider cannot honor the requested value.
type chatFieldCheck struct {
	param       string
	unsupported func() bool
}

// chatFieldChecks lists the per-field checks in documented field order.
// Codex ignores sampling controls, output caps, stop sequences, forced
// tool choice, and structured output formats. Anthropic clamps
// temperature above 1 and has no processing tier. Neither provider reads
// n, penalties, logit bias, logprobs, seed, audio, non-text modalities,
// stored completions, or the legacy function_call control.
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

// presenceSet reports whether the request set a key to a non-null value.
func presenceSet(presence int) bool {
	return presence != chatPresenceAbsent && presence != chatPresenceNull
}

// valueSet reports whether the request set a key to a non-empty value.
// An empty string, array, or object has the same meaning as omission.
func valueSet(presence int) bool {
	return presenceSet(presence) && presence != chatPresenceEmpty
}

// numberDiffers reports whether a numeric field differs from the
// documented default. An omitted field keeps the default.
func numberDiffers(value *float64, documentedDefault float64) bool {
	return value != nil && *value != documentedDefault
}

// temperatureUnsupported reports whether the provider cannot honor the
// requested temperature. Codex ignores temperature and honors only the
// default of 1. Anthropic clamps temperature to the range 0 to 1.
func temperatureUnsupported(column providerColumn, temperature *float64) bool {
	if temperature == nil {
		return false
	}
	if column == columnCodex {
		return *temperature != 1
	}
	return *temperature > 1
}

// modalitiesUnsupported reports whether the request asks for an output
// modality other than text.
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

// defaultServiceTiers are the service_tier values that select the
// default processing tier.
var defaultServiceTiers = map[string]bool{
	"":        true,
	"auto":    true,
	"default": true,
}

// serviceTierUnsupported reports whether the request asks for a
// processing tier other than the default.
func serviceTierUnsupported(tier string) bool {
	return !defaultServiceTiers[strings.TrimSpace(tier)]
}

// choiceIsAuto reports whether a tool_choice or function_call value
// selects the default automatic behavior.
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

// responseFormatIsText reports whether response_format selects plain
// text output.
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
