package mitmcontrib

import (
	"log/slog"

	"goodkind.io/clyde/internal/logevent"
)

// FacetKey is the typed wire key under which Cursor identity
// metadata appears on shared request events. Production code MUST
// reference this constant; the generic [logevent] package does not
// know it.
const FacetKey = "cursor"

// Facet is the typed Cursor identity contribution attached to a
// shared [logevent.Event] when the MITM proxy intercepts Cursor
// traffic. The struct owns its own wire shape; the generic emitter
// handles it only through the [logevent.Facet] interface.
type Facet struct {
	RequestID         string `json:"request_id,omitempty"`
	ConversationID    string `json:"conversation_id,omitempty"`
	GenerationID      string `json:"generation_id,omitempty"`
	OriginalRequestID string `json:"original_request_id,omitempty"`
	SessionID         string `json:"session_id,omitempty"`
}

// FacetKey returns the typed wire key for the Cursor facet.
func (f Facet) FacetKey() string { return FacetKey }

// FacetAttrs returns the slog attributes flushed under the Cursor
// facet group in the canonical request event.
func (f Facet) FacetAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, 5)
	if f.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", f.RequestID))
	}
	if f.ConversationID != "" {
		attrs = append(attrs, slog.String("conversation_id", f.ConversationID))
	}
	if f.GenerationID != "" {
		attrs = append(attrs, slog.String("generation_id", f.GenerationID))
	}
	if f.OriginalRequestID != "" {
		attrs = append(attrs, slog.String("original_request_id", f.OriginalRequestID))
	}
	if f.SessionID != "" {
		attrs = append(attrs, slog.String("session_id", f.SessionID))
	}
	return attrs
}

// SinkHints reports the Cursor facet contributes no extra sink
// routing on its own. Cursor traffic flows through the MITM surface.
func (f Facet) SinkHints() logevent.SinkHints {
	return logevent.SinkHints{
		NeedsProviderSidecar: false,
	}
}

// Empty reports whether the facet carries any populated field.
func (f Facet) Empty() bool {
	return f.RequestID == "" &&
		f.ConversationID == "" &&
		f.GenerationID == "" &&
		f.OriginalRequestID == "" &&
		f.SessionID == ""
}
