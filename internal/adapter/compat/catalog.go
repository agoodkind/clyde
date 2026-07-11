package compat

import (
	"encoding/json"
	"strings"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

// disposition is what the resolved provider does with a request field.
// Translate carries the field through; OmitWarn drops it and warns;
// OverrideWarn replaces its value and warns.
type disposition int

const (
	dispositionTranslate disposition = iota
	dispositionOmitWarn
	dispositionOverrideWarn
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
	dispositionLabelOmitted    = "omitted"
	dispositionLabelOverridden = "overridden"
)

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
	{param: "max_output_tokens", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "temperature", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "top_p", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "stop", codex: dispositionOmitWarn, anthropic: dispositionTranslate},
	{param: "store", codex: dispositionOverrideWarn, anthropic: dispositionOmitWarn},
	{param: "include", codex: dispositionTranslate, anthropic: dispositionOmitWarn},
	{param: "service_tier", codex: dispositionTranslate, anthropic: dispositionOmitWarn},
	{param: "truncation", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
	{param: "prompt_cache_retention", codex: dispositionOmitWarn, anthropic: dispositionOmitWarn},
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
		return CompatibilityWarning{
			Code:        warningCodeOverridden,
			Param:       entry.param,
			Disposition: dispositionLabelOverridden,
			Message:     entry.param + " is not supported by the " + backend + " backend and is forced to false",
		}
	case dispositionTranslate:
		return CompatibilityWarning{Code: "", Param: "", Disposition: "", Message: ""}
	default:
		return CompatibilityWarning{Code: "", Param: "", Disposition: "", Message: ""}
	}
}

// fieldPresence classifies how a top-level request field appears in the
// raw body. Absent and Null suppress warnings; Empty, Zero, and Present
// all count as present enough for the catalog to apply.
type fieldPresence int

const (
	presenceAbsent fieldPresence = iota
	presenceNull
	presenceEmpty
	presenceZero
	presencePresent
)

// presence classifies one raw field value. A missing key is passed as an
// empty raw value and classifies as Absent. A literal null classifies as
// Null and is treated as Absent for warnings, because a client that sends
// null explicitly cleared the field.
func presence(raw json.RawMessage) fieldPresence {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return presenceAbsent
	}
	if trimmed == "null" {
		return presenceNull
	}
	if trimmed == `""` || trimmed == "[]" || trimmed == "{}" {
		return presenceEmpty
	}
	if trimmed == "0" {
		return presenceZero
	}
	return presencePresent
}

// presenceWarns reports whether a presence class should trigger a warning.
// Every class except Absent and Null counts as present, so a zero, false,
// or empty value still warns.
func presenceWarns(class fieldPresence) bool {
	return class != presenceAbsent && class != presenceNull
}

// parseTopLevelKeys decodes only the top-level object keys of the request
// body into raw values. This map is the single documented opaque edge in
// the package: [json.RawMessage] values let the boundary classify field
// presence without modeling every Responses field. A malformed body yields
// an empty map, so no warnings are produced.
func parseTopLevelKeys(body []byte) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	if len(body) == 0 {
		return fields
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		return map[string]json.RawMessage{}
	}
	return fields
}
