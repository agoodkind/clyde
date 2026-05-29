package anthropic

import (
	"log/slog"

	"goodkind.io/clyde/internal/adapter/backendfacet"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	"goodkind.io/clyde/internal/logevent"
)

// FacetKey is the typed wire key under which Anthropic telemetry
// appears in shared request events. Production code MUST reference
// this constant; the generic [logevent] package does not know it.
const FacetKey = "anthropic"

// Facet is the typed Anthropic contribution attached to a shared
// [logevent.Event] by adapter code that dispatches to the
// Anthropic upstream. The struct owns its own wire shape; the
// generic emitter handles it only through the [logevent.Facet]
// interface.
type Facet struct {
	Model            string `json:"model,omitempty"`
	ThinkingEnabled  bool   `json:"thinking_enabled,omitempty"`
	StopReason       string `json:"stop_reason,omitempty"`
	StreamEventCount int    `json:"stream_event_count,omitempty"`
	RetryAttempt     int    `json:"retry_attempt,omitempty"`
}

// FacetKey returns the typed wire key for the Anthropic facet.
func (f Facet) FacetKey() string { return FacetKey }

// FacetAttrs returns the slog attributes flushed under the
// Anthropic facet group in the canonical request event.
func (f Facet) FacetAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 5)
	if f.Model != "" {
		attrs = append(attrs, slog.String("model", f.Model))
	}
	if f.ThinkingEnabled {
		attrs = append(attrs, slog.Bool("thinking_enabled", true))
	}
	if f.StopReason != "" {
		attrs = append(attrs, slog.String("stop_reason", f.StopReason))
	}
	if f.StreamEventCount > 0 {
		attrs = append(attrs, slog.Int("stream_event_count", f.StreamEventCount))
	}
	if f.RetryAttempt > 0 {
		attrs = append(attrs, slog.Int("retry_attempt", f.RetryAttempt))
	}
	return attrs
}

// SinkHints reports that Anthropic telemetry mirrors to the
// provider sidecar sink.
func (f Facet) SinkHints() logevent.SinkHints {
	return logevent.SinkHints{
		HasRawCapture:        false,
		NeedsProviderSidecar: true,
	}
}

// RegisterBackendFacet installs the Anthropic facet factory into the
// adapter logging registry.
func RegisterBackendFacet(reg backendfacet.Registrar) {
	if reg == nil {
		return
	}
	reg.Register(adaptermodel.BackendAnthropic.String(), backendfacet.FactoryFunc(newBackendFacet))
}

func newBackendFacet(input backendfacet.Input) logevent.Facet {
	return Facet{
		Model:            input.Model,
		ThinkingEnabled:  input.ThinkingEnabled,
		StopReason:       "",
		StreamEventCount: input.StreamEventCount,
		RetryAttempt:     input.RetryAttempt,
	}
}
