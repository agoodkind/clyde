package compat

import adaptermodel "goodkind.io/clyde/internal/adapter/model"

// disposition is what the resolved provider does with a request field.
// Translate carries the field through; OmitWarn drops it and warns;
// OverrideWarn replaces its value and warns.
type disposition int

const (
	dispositionTranslate disposition = iota
	dispositionOmitWarn
	dispositionOverrideWarn
	dispositionReject
	dispositionPartial
)

// providerColumn selects which catalog column a resolved provider reads.
// Claude and Anthropic share the Anthropic column.
type providerColumn int

const (
	columnCodex providerColumn = iota
	columnAnthropic
)

// warning code and disposition label values carried on CompatibilityWarning.
const (
	warningCodeOmitted         = "field_omitted"
	warningCodeOverridden      = "field_overridden"
	warningCodeToolUnsupported = "tool_unsupported"
	dispositionLabelOmitted    = "omitted"
	dispositionLabelOverridden = "overridden"
)

// toolUnsupportedWarning builds the warning for one dropped Responses tool
// type. The type label is a bounded tool-kind discriminator that the
// openai classifier assigns (function tools are kept; built-in and custom
// tools are dropped by their type), so it is safe to include in the
// message. The projection performs the drop; this boundary only describes
// it.
func toolUnsupportedWarning(toolType string) CompatibilityWarning {
	return CompatibilityWarning{
		Code:        warningCodeToolUnsupported,
		Param:       "tools",
		Disposition: dispositionLabelOmitted,
		Message:     "tool type " + toolType + " is not supported and was omitted",
	}
}

// catalogEntry is one field's per-provider disposition.
type catalogEntry struct {
	param     string
	codex     disposition
	anthropic disposition
}

// dispositionFor returns the entry's disposition for the given column.
func (e catalogEntry) dispositionFor(column providerColumn) disposition {
	if column == columnCodex {
		return e.codex
	}
	return e.anthropic
}

// responsesCatalog lists the Responses request fields whose disposition
// differs by provider, in ResponsesRequest field order. Raw-body key order
// is not stable, so this fixed order is the canonical warning order. Fields
// that Translate for both providers never warn and are omitted here.
var responsesCatalog = []catalogEntry{
	{param: "previous_response_id", codex: dispositionReject, anthropic: dispositionReject},
	{param: "model", codex: dispositionTranslate, anthropic: dispositionTranslate},
	{param: "background", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "max_tool_calls", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "text", codex: dispositionPartial, anthropic: dispositionOmitWarn},
	{param: "tools", codex: dispositionPartial, anthropic: dispositionPartial},
	{param: "tool_choice", codex: dispositionPartial, anthropic: dispositionPartial},
	{param: "prompt", codex: dispositionReject, anthropic: dispositionReject},
	{param: "prompt_cache_options", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "top_logprobs", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "metadata", codex: dispositionPartial, anthropic: dispositionOmitWarn},
	{param: "temperature", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "top_p", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "user", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "safety_identifier", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "prompt_cache_key", codex: dispositionOverrideWarn, anthropic: dispositionOmitWarn},
	{param: "service_tier", codex: dispositionTranslate, anthropic: dispositionOmitWarn},
	{param: "prompt_cache_retention", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "truncation", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "reasoning", codex: dispositionTranslate, anthropic: dispositionPartial},
	{param: "input", codex: dispositionTranslate, anthropic: dispositionPartial},
	{param: "include", codex: dispositionPartial, anthropic: dispositionOmitWarn},
	{param: "parallel_tool_calls", codex: dispositionTranslate, anthropic: dispositionOmitWarn},
	{param: "store", codex: dispositionOverrideWarn, anthropic: dispositionOmitWarn},
	{param: "instructions", codex: dispositionTranslate, anthropic: dispositionTranslate},
	{param: "moderation", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "stream", codex: dispositionTranslate, anthropic: dispositionTranslate},
	{param: "stream_options", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "conversation", codex: dispositionReject, anthropic: dispositionReject},
	{param: "context_management", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "max_output_tokens", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "max_tokens", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "max_completion_tokens", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "n", codex: dispositionPartial, anthropic: dispositionPartial},
	{param: "stop", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
}

// providerColumnFor maps a resolved backend to its catalog column. Codex
// reads the Codex column; Anthropic and Claude read the Anthropic column.
// Passthrough and unknown providers report false so no warnings are emitted.
func providerColumnFor(provider adaptermodel.BackendID) (providerColumn, bool) {
	switch provider {
	case adaptermodel.BackendCodex:
		return columnCodex, true
	case adaptermodel.BackendAnthropic, adaptermodel.BackendClaude:
		return columnAnthropic, true
	case adaptermodel.BackendPassthroughOverride:
		return columnCodex, false
	default:
		return columnCodex, false
	}
}

// backendLabel is the sanitized provider name used in warning messages.
func backendLabel(column providerColumn) string {
	if column == columnCodex {
		return "codex"
	}
	return "anthropic"
}

// warningFor builds the warning for a warnable catalog entry under the
// given column. Message text is generic and never carries a request value.
func warningFor(entry catalogEntry, column providerColumn) CompatibilityWarning {
	backend := backendLabel(column)
	switch entry.dispositionFor(column) {
	case dispositionOmitWarn:
		return CompatibilityWarning{
			Code:        warningCodeOmitted,
			Param:       entry.param,
			Disposition: dispositionLabelOmitted,
			Message:     entry.param + " is not supported by the " + backend + " backend and was omitted",
		}
	case dispositionOverrideWarn:
		message := entry.param + " is not supported by the " + backend + " backend and is forced to false"
		if column == columnCodex && entry.param == "prompt_cache_key" {
			message = "prompt_cache_key is replaced with Clyde-owned cache identity for the codex backend"
		}
		return CompatibilityWarning{
			Code:        warningCodeOverridden,
			Param:       entry.param,
			Disposition: dispositionLabelOverridden,
			Message:     message,
		}
	case dispositionTranslate:
		return CompatibilityWarning{Code: "", Param: "", Disposition: "", Message: ""}
	case dispositionReject, dispositionPartial:
		return CompatibilityWarning{Code: "", Param: "", Disposition: "", Message: ""}
	default:
		return CompatibilityWarning{Code: "", Param: "", Disposition: "", Message: ""}
	}
}
