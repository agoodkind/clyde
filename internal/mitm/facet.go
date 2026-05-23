package mitm

import (
	"log/slog"

	"goodkind.io/clyde/internal/logevent"
)

// FacetKey is the typed wire key under which MITM transport
// telemetry appears in shared request events. The MITM facet is
// transport-shaped, not provider-shaped; no field on the struct
// names a specific upstream provider.
const FacetKey = "mitm"

// Facet is the typed MITM-surface contribution attached to a
// shared [logevent.Event] by code under internal/mitm. The struct
// describes the transport surface that proxied the request and the
// raw capture sidecar paths a downstream tool can read.
type Facet struct {
	Concern             string `json:"concern,omitempty"`
	Transport           string `json:"transport,omitempty"`
	Direction           string `json:"direction,omitempty"`
	Sequence            int    `json:"sequence,omitempty"`
	CloseReason         string `json:"close_reason,omitempty"`
	RequestContentType  string `json:"request_content_type,omitempty"`
	ResponseContentType string `json:"response_content_type,omitempty"`
	CapturePath         string `json:"capture_path,omitempty"`
	RawRequestPath      string `json:"raw_request_path,omitempty"`
	RawResponsePath     string `json:"raw_response_path,omitempty"`
}

// FacetKey returns the typed wire key for the MITM facet.
func (f Facet) FacetKey() string { return FacetKey }

// FacetAttrs returns the slog attributes flushed under the MITM
// facet group in the canonical request event.
func (f Facet) FacetAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 10)
	if f.Concern != "" {
		attrs = append(attrs, slog.String("concern", f.Concern))
	}
	if f.Transport != "" {
		attrs = append(attrs, slog.String("transport", f.Transport))
	}
	if f.Direction != "" {
		attrs = append(attrs, slog.String("direction", f.Direction))
	}
	if f.Sequence > 0 {
		attrs = append(attrs, slog.Int("sequence", f.Sequence))
	}
	if f.CloseReason != "" {
		attrs = append(attrs, slog.String("close_reason", f.CloseReason))
	}
	if f.RequestContentType != "" {
		attrs = append(attrs, slog.String("request_content_type", f.RequestContentType))
	}
	if f.ResponseContentType != "" {
		attrs = append(attrs, slog.String("response_content_type", f.ResponseContentType))
	}
	if f.CapturePath != "" {
		attrs = append(attrs, slog.String("capture_path", f.CapturePath))
	}
	if f.RawRequestPath != "" {
		attrs = append(attrs, slog.String("raw_request_path", f.RawRequestPath))
	}
	if f.RawResponsePath != "" {
		attrs = append(attrs, slog.String("raw_response_path", f.RawResponsePath))
	}
	return attrs
}

// SinkHints reports the typed sink-routing hints the MITM facet
// contributes. RawRequestPath or RawResponsePath being non-empty
// implies the event references raw capture sidecars that must
// mirror to the raw sink.
func (f Facet) SinkHints() logevent.SinkHints {
	return logevent.SinkHints{
		HasRawCapture:        f.RawRequestPath != "" || f.RawResponsePath != "",
		NeedsProviderSidecar: false,
	}
}
