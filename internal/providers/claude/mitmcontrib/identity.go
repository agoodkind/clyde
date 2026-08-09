package mitmcontrib

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

const conversationSourceHeader = "header"

// extractIdentity parses Claude Code session headers from the request
// header bundle and returns the typed contribution the generic MITM layer
// attaches to capture rows and wire logs.
func extractIdentity(headers http.Header) mitm.IdentityContribution {
	sessionID := firstHeader(headers, "x-claude-code-session-id")
	requestID := firstHeader(headers, "x-client-request-id")
	contrib := mitm.IdentityContribution{
		PreferredRequestID: requestID,
		SessionID:          sessionID,
	}
	if sessionID != "" {
		contrib.ConversationID = sessionID
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
