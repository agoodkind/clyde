package codex

import (
	"log/slog"

	"goodkind.io/clyde/internal/adapter/backendfacet"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/logevent"
)

// FacetKey is the typed wire key under which Codex telemetry
// appears in shared request events. Production code MUST reference
// this constant; the generic [logevent] package does not know it.
const FacetKey = "codex"

// Facet is the typed Codex contribution attached to a shared
// [logevent.Event] by adapter code that dispatches to the Codex
// upstream. The struct owns its own wire shape; the generic event
// emitter handles it only through the [logevent.Facet] interface.
type Facet struct {
	Model                 string `json:"model,omitempty"`
	Effort                string `json:"effort,omitempty"`
	ReasoningSummary      string `json:"reasoning_summary,omitempty"`
	Transport             string `json:"transport,omitempty"`
	ServiceTier           string `json:"service_tier,omitempty"`
	PreviousResponseID    bool   `json:"previous_response_id,omitempty"`
	PromptCacheKeyPresent bool   `json:"prompt_cache_key_present,omitempty"`
	InputCount            int    `json:"input_count,omitempty"`
	ToolCount             int    `json:"tool_count,omitempty"`
	StreamEventCount      int    `json:"stream_event_count,omitempty"`
	RetryAttempt          int    `json:"retry_attempt,omitempty"`
}

// FacetKey returns the typed wire key for the Codex facet.
func (f Facet) FacetKey() string { return FacetKey }

// FacetAttrs returns the slog attributes flushed under the Codex
// facet group in the canonical request event.
func (f Facet) FacetAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 11)
	if f.Model != "" {
		attrs = append(attrs, slog.String("model", f.Model))
	}
	if f.Effort != "" {
		attrs = append(attrs, slog.String("effort", f.Effort))
	}
	if f.ReasoningSummary != "" {
		attrs = append(attrs, slog.String("reasoning_summary", f.ReasoningSummary))
	}
	if f.Transport != "" {
		attrs = append(attrs, slog.String("transport", f.Transport))
	}
	if f.ServiceTier != "" {
		attrs = append(attrs, slog.String("service_tier", f.ServiceTier))
	}
	if f.PreviousResponseID {
		attrs = append(attrs, slog.Bool("previous_response_id", true))
	}
	if f.PromptCacheKeyPresent {
		attrs = append(attrs, slog.Bool("prompt_cache_key_present", true))
	}
	if f.InputCount > 0 {
		attrs = append(attrs, slog.Int("input_count", f.InputCount))
	}
	if f.ToolCount > 0 {
		attrs = append(attrs, slog.Int("tool_count", f.ToolCount))
	}
	if f.StreamEventCount > 0 {
		attrs = append(attrs, slog.Int("stream_event_count", f.StreamEventCount))
	}
	if f.RetryAttempt > 0 {
		attrs = append(attrs, slog.Int("retry_attempt", f.RetryAttempt))
	}
	return attrs
}

// SinkHints reports that Codex telemetry mirrors to the provider
// sidecar sink. The hint is provider-owned; the generic sink
// selector reads only the aggregated typed [logevent.SinkHints].
func (f Facet) SinkHints() logevent.SinkHints {
	return logevent.SinkHints{
		NeedsProviderSidecar: true,
	}
}

// RegisterBackendFacet installs the Codex facet factory into the
// adapter logging registry.
func RegisterBackendFacet(reg backendfacet.Registrar) {
	if reg == nil {
		return
	}
	reg.Register(adaptermodel.BackendCodex.String(), backendfacet.FactoryFunc(newBackendFacet))
}

func newBackendFacet(input backendfacet.Input) logevent.Facet {
	return Facet{
		Model:                 input.Model,
		Effort:                input.Effort,
		ReasoningSummary:      input.ReasoningSummary,
		Transport:             "",
		ServiceTier:           input.ServiceTier,
		PreviousResponseID:    false,
		PromptCacheKeyPresent: false,
		InputCount:            input.InputCount,
		ToolCount:             input.ToolCount,
		StreamEventCount:      input.StreamEventCount,
		RetryAttempt:          input.RetryAttempt,
	}
}
