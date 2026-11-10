package mitmcontrib

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

// extractIdentity parses the Cursor identity headers off the request
// header bundle and returns the typed contribution the generic MITM
// layer attaches to the emitted event. The header key set is the
// union of the Cursor production header schema and legacy aliases
// the Cursor CLI emitted before the public rename so existing
// capture fixtures and live traffic both populate the facet.
func extractIdentity(headers http.Header) mitm.IdentityContribution {
	requestID := firstHeader(headers, "x-request-id", "x-cursor-request-id", "request-id")
	originalRequestID := firstHeader(headers, "x-original-request-id", "x-cursor-original-request-id", "original-request-id")
	sessionID := firstHeader(headers, "x-session-id", "x-cursor-session-id", "cursor-session-id")
	conversationID := firstHeader(headers, "x-cursor-conversation-id", "x-conversation-id")
	generationID := firstHeader(headers, "x-cursor-generation-id", "x-generation-id")
	facet := Facet{
		RequestID:         requestID,
		ConversationID:    conversationID,
		GenerationID:      generationID,
		OriginalRequestID: originalRequestID,
		SessionID:         sessionID,
	}
	contrib := mitm.IdentityContribution{
		PreferredRequestID:         requestID,
		PreferredUpstreamRequestID: originalRequestID,
		SessionID:                  sessionID,
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
	if conversationID != "" {
		contrib.ConversationID = conversationID
		contrib.ConversationSource = "header"
	}
	if !facet.Empty() {
		contrib.Facet = facet
	}
	return contrib
}

func firstHeader(h http.Header, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(h.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

// CaptureHeaders is a typed helper exposing raw Cursor identity
// headers for callers inside this package that still need the raw
// values during the intercepted-request capture flow.
type CaptureHeaders struct {
	RequestID         string
	OriginalRequestID string
	SessionID         string
	Traceparent       string
}

// ExtractCaptureHeaders returns the raw Cursor identity headers
// from the supplied header bundle.
func ExtractCaptureHeaders(h http.Header) CaptureHeaders {
	return CaptureHeaders{
		RequestID:         firstHeader(h, "x-request-id", "x-cursor-request-id", "request-id"),
		OriginalRequestID: firstHeader(h, "x-original-request-id", "x-cursor-original-request-id", "original-request-id"),
		SessionID:         firstHeader(h, "x-session-id", "x-cursor-session-id", "cursor-session-id"),
		Traceparent:       firstHeader(h, "traceparent"),
	}
}
