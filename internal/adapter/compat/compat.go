// Package compat computes per-provider field-disposition warnings for an
// inbound adapter request. It is a description-only boundary: it reports
// which request fields the resolved provider will omit or override, and it
// never performs the omission itself and never carries request values,
// prompts, credentials, or provider bodies.
package compat

import (
	"encoding/json"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
)

// Endpoint is the OpenAI-compatible route dialect a request arrived on.
type Endpoint int

const (
	// EndpointChat is the /v1/chat/completions dialect. Chat behavior is
	// unchanged, so this endpoint produces no warnings today.
	EndpointChat Endpoint = iota
	// EndpointResponses is the /v1/responses dialect the warning catalog
	// describes.
	EndpointResponses
)

// CompatibilityWarning is one field-level compatibility notice. It never
// carries request values, prompts, credentials, or provider bodies.
type CompatibilityWarning struct {
	Code        string `json:"code"`        // e.g. "field_omitted", "field_overridden"
	Param       string `json:"param"`       // the request field name, e.g. "temperature"
	Disposition string `json:"disposition"` // "omitted" | "overridden"
	Message     string `json:"message"`     // sanitized, no request values
}

// WarningSet is the ordered, de-duplicated, bounded set of warnings for
// one request. The zero value is a valid empty set.
type WarningSet struct {
	warnings []CompatibilityWarning
}

const (
	// maxWarnings bounds how many warnings one request can carry.
	maxWarnings = 32
	// maxHeaderBytes bounds the summed byte length of Headers().
	maxHeaderBytes = 8 * 1024
)

// ComputeWarningsFromResponsesPresence keeps the decoded n value beside the
// presence callback because n warns only when it asks for more than one result.
func ComputeWarningsFromResponsesPresence(presenceFor func(string) int, n *int, provider adaptermodel.BackendID, endpoint Endpoint, unsupportedTools []string) WarningSet {
	if endpoint != EndpointResponses {
		return WarningSet{warnings: nil}
	}
	column, known := providerColumnFor(provider)
	if !known {
		return WarningSet{warnings: nil}
	}
	raw := make([]CompatibilityWarning, 0, len(responsesCatalog)+len(unsupportedTools))
	for _, entry := range responsesCatalog {
		if entry.param == "n" {
			if n != nil && *n > 1 {
				raw = append(raw, CompatibilityWarning{Code: warningCodeOmitted, Param: "n", Disposition: dispositionLabelOmitted, Message: "n is not supported by the " + backendLabel(column) + " backend and one result was returned"})
			}
			continue
		}
		if entry.dispositionFor(column) != dispositionOmitWarn && entry.dispositionFor(column) != dispositionOverrideWarn {
			continue
		}
		presence := presenceFor(entry.param)
		if presence == 0 || presence == 1 {
			continue
		}
		raw = append(raw, warningFor(entry, column))
	}
	for _, toolType := range unsupportedTools {
		raw = append(raw, toolUnsupportedWarning(toolType))
	}
	return newWarningSet(raw)
}

// RejectedParam returns the first OpenAI-owned reference field that Clyde
// cannot resolve. It uses catalog order so the selected error is stable.
func RejectedParam(presenceFor func(string) int) string {
	for _, entry := range responsesCatalog {
		if entry.codex != dispositionReject {
			continue
		}
		presence := presenceFor(entry.param)
		if presence != 0 && presence != 1 {
			return entry.param
		}
	}
	return ""
}

// Empty reports whether the set carries no warnings.
func (s WarningSet) Empty() bool {
	return len(s.warnings) == 0
}

// Slice returns the warnings in canonical request-field order, deduped and
// capped. The returned slice is owned by the set; callers must not mutate it.
func (s WarningSet) Slice() []CompatibilityWarning {
	return s.warnings
}

// Headers returns one compact-JSON string per warning, suitable for a
// repeated response header. The summed byte length is bounded by the same
// cap applied when the set was built.
func (s WarningSet) Headers() []string {
	if len(s.warnings) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.warnings))
	for _, warning := range s.warnings {
		out = append(out, warningHeader(warning))
	}
	return out
}

// newWarningSet dedups by (Code, Param) and applies the count and byte
// caps, preserving canonical request-field order.
func newWarningSet(raw []CompatibilityWarning) WarningSet {
	return WarningSet{warnings: capWarnings(dedupWarnings(raw))}
}

// warningKey identifies a warning for dedup by its code, param, and a
// discriminator. Field warnings dedup by (code, param) alone; tool
// warnings all share the tools param, so the discriminator carries the
// dropped tool type (via the message) to keep distinct types distinct
// while collapsing repeated identical types.
type warningKey struct {
	code  string
	param string
	disc  string
}

// dedupKey builds the dedup key for one warning. The discriminator stays
// empty for field warnings so their behavior is unchanged, and carries the
// type-bearing message for tool warnings so distinct tool types survive.
func dedupKey(warning CompatibilityWarning) warningKey {
	disc := ""
	if warning.Code == warningCodeToolUnsupported {
		disc = warning.Message
	}
	return warningKey{code: warning.Code, param: warning.Param, disc: disc}
}

// dedupWarnings keeps the first warning for each dedup key in input order.
func dedupWarnings(in []CompatibilityWarning) []CompatibilityWarning {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[warningKey]bool, len(in))
	out := make([]CompatibilityWarning, 0, len(in))
	for _, warning := range in {
		key := dedupKey(warning)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, warning)
	}
	return out
}

// capWarnings drops overflow deterministically, keeping the earliest
// warnings in canonical order until either the count cap or the summed
// header byte budget would be exceeded.
func capWarnings(in []CompatibilityWarning) []CompatibilityWarning {
	if len(in) == 0 {
		return nil
	}
	out := make([]CompatibilityWarning, 0, len(in))
	total := 0
	for _, warning := range in {
		if len(out) >= maxWarnings {
			break
		}
		header := warningHeader(warning)
		if total+len(header) > maxHeaderBytes {
			break
		}
		total += len(header)
		out = append(out, warning)
	}
	return out
}

// warningHeader renders one warning as compact JSON for a header value.
func warningHeader(warning CompatibilityWarning) string {
	encoded, err := json.Marshal(warning)
	if err != nil {
		return ""
	}
	return string(encoded)
}
