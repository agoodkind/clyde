package compat

import (
	"encoding/json"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

// ResponsesRequestValues are the decoded Responses values that decide a
// rejection by value instead of by presence alone.
type ResponsesRequestValues struct {
	N             *int
	ToolChoice    json.RawMessage
	Temperature   *float64
	TopP          *float64
	TopLogprobs   *int
	Background    *bool
	Store         *bool
	ParallelTools *bool
	Truncation    *string
	ServiceTier   *string
	UnknownKeys   []string
}

// responsesHintParams are Responses fields that tune caching, abuse
// monitoring, request bookkeeping, or stream obfuscation. Omitting them
// does not change the generated output. A provider that drops them still
// honors the request.
var responsesHintParams = map[string]bool{
	"prompt_cache_key":       true,
	"prompt_cache_options":   true,
	"prompt_cache_retention": true,
	"user":                   true,
	"safety_identifier":      true,
	"metadata":               true,
	"stream_options":         true,
}

// ResponsesRejection returns the first Responses request field that the
// resolved provider cannot honor as the Responses contract documents it.
// It reads the same per-provider catalog as the compatibility warnings.
// A field that the provider omits or overrides produces a rejection
// unless the field is a hint or its value equals the documented default.
// unsupportedTools lists the tool types the projection cannot pass to the
// provider.
func ResponsesRejection(presenceFor func(string) int, values ResponsesRequestValues, provider adaptermodel.BackendID, unsupportedTools []string) (Rejection, bool) {
	if len(values.UnknownKeys) > 0 {
		key := values.UnknownKeys[0]
		return Rejection{
			Code:    RejectionCodeUnknownParameter,
			Param:   key,
			Message: "Unrecognized request argument supplied: " + key,
		}, true
	}
	column, known := providerColumnFor(provider)
	if !known {
		return noRejection(), false
	}
	for _, entry := range responsesCatalog {
		if !valueSet(presenceFor(entry.param)) {
			continue
		}
		if responsesFieldUnsupported(entry, column, values, unsupportedTools) {
			return unsupportedParameter(entry.param, column), true
		}
	}
	return noRejection(), false
}

// responsesFieldUnsupported applies one catalog entry to the request.
func responsesFieldUnsupported(entry catalogEntry, column providerColumn, values ResponsesRequestValues, unsupportedTools []string) bool {
	switch entry.dispositionFor(column) {
	case dispositionTranslate:
		return entry.param == "temperature" && temperatureUnsupported(column, values.Temperature)
	case dispositionOmitWarn, dispositionOverrideWarn:
		if responsesHintParams[entry.param] {
			return false
		}
		return !responsesValueIsDocumentedDefault(entry.param, values)
	case dispositionPartial:
		return responsesPartialUnsupported(entry.param, column, values, unsupportedTools)
	case dispositionReject:
		return true
	default:
		return true
	}
}

// responsesPartialUnsupported reports whether a partially supported
// field uses a value the provider cannot honor. A partial field without a
// value check is honored.
func responsesPartialUnsupported(param string, column providerColumn, values ResponsesRequestValues, unsupportedTools []string) bool {
	checks := map[string]func() bool{
		"n":           func() bool { return values.N != nil && *values.N > 1 },
		"tools":       func() bool { return len(unsupportedTools) > 0 },
		"tool_choice": func() bool { return column == columnCodex && !choiceIsAuto(values.ToolChoice) },
	}
	check, ok := checks[param]
	return ok && check()
}

// responsesDocumentedDefaults maps a Responses field to the predicate that
// reports whether the request value equals the documented default. A
// provider that drops a field set to its default still produces the
// documented behavior.
var responsesDocumentedDefaults = map[string]func(ResponsesRequestValues) bool{
	"temperature":  func(values ResponsesRequestValues) bool { return !numberDiffers(values.Temperature, 1) },
	"top_p":        func(values ResponsesRequestValues) bool { return !numberDiffers(values.TopP, 1) },
	"top_logprobs": func(values ResponsesRequestValues) bool { return values.TopLogprobs == nil || *values.TopLogprobs == 0 },
	"background":   func(values ResponsesRequestValues) bool { return values.Background == nil || !*values.Background },
	"store":        func(values ResponsesRequestValues) bool { return values.Store != nil && !*values.Store },
	"parallel_tool_calls": func(values ResponsesRequestValues) bool {
		return values.ParallelTools == nil || *values.ParallelTools
	},
	"truncation": func(values ResponsesRequestValues) bool {
		return values.Truncation == nil || strings.TrimSpace(*values.Truncation) == "disabled"
	},
	"service_tier": func(values ResponsesRequestValues) bool {
		return values.ServiceTier == nil || !serviceTierUnsupported(*values.ServiceTier)
	},
}

// responsesValueIsDocumentedDefault reports whether a request value
// equals the documented default. A field without a default predicate has
// no value that a dropping provider can honor.
func responsesValueIsDocumentedDefault(param string, values ResponsesRequestValues) bool {
	isDefault, ok := responsesDocumentedDefaults[param]
	return ok && isDefault(values)
}
