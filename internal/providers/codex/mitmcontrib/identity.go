package mitmcontrib

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

const conversationSourceHeader = "header"

// extractIdentity parses Codex session and thread headers from the request
// header bundle and returns the typed contribution the generic MITM layer
// attaches to capture rows and wire logs. Header names match live Codex CLI
// traffic on /backend-api/codex/responses.
func extractIdentity(headers http.Header) mitm.IdentityContribution {
	sessionID := firstHeader(headers, "session-id", "thread-id")
	threadID := firstHeader(headers, "thread-id", "session-id")
	requestID := firstHeader(headers, "x-client-request-id")
	contrib := mitm.IdentityContribution{
		PreferredRequestID:         requestID,
		PreferredUpstreamRequestID: "",
		SessionID:                  sessionID,
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
	if threadID != "" {
		contrib.ConversationID = threadID
		contrib.ConversationSource = conversationSourceHeader
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
