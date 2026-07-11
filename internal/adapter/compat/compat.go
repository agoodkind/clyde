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

// ComputeWarnings returns the warning set for one request body under the
// resolved provider and route dialect. It parses the body once for
// top-level key presence, walks the fixed catalog in canonical order, and
// returns an empty set for the chat dialect, the passthrough provider, or
// an unknown provider.
func ComputeWarnings(body []byte, provider adaptermodel.BackendID, endpoint Endpoint) WarningSet {
	switch endpoint {
	case EndpointChat:
		return WarningSet{warnings: nil}
	case EndpointResponses:
		// Responses is the only dialect with a warning catalog today.
	default:
		return WarningSet{warnings: nil}
	}
	column, known := providerColumnFor(provider)
	if !known {
		return WarningSet{warnings: nil}
	}
	fields := parseTopLevelKeys(body)
	raw := make([]CompatibilityWarning, 0, len(responsesCatalog))
	for _, entry := range responsesCatalog {
		if entry.dispositionFor(column) == dispositionTranslate {
			continue
		}
		if !presenceWarns(presence(fields[entry.param])) {
			continue
		}
		raw = append(raw, warningFor(entry, column))
	}
	return newWarningSet(raw)
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

// warningKey identifies a warning for dedup by its code and param.
type warningKey struct {
	code  string
	param string
}

// dedupWarnings keeps the first warning for each (Code, Param) pair in
// input order.
func dedupWarnings(in []CompatibilityWarning) []CompatibilityWarning {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[warningKey]bool, len(in))
	out := make([]CompatibilityWarning, 0, len(in))
	for _, warning := range in {
		key := warningKey{code: warning.Code, param: warning.Param}
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
