package mitmcontrib

import (
	"net/http"
	"testing"
)

func TestExtractIdentityClaudeHeaders(t *testing.T) {
	headers := http.Header{
		"X-Claude-Code-Session-Id": []string{"b3e8f88e-3fe8-421f-9cb0-e327103d0a4e"},
		"X-Client-Request-Id":      []string{"req-claude-1"},
	}
	contrib := extractIdentity(headers)
	if contrib.SessionID != "b3e8f88e-3fe8-421f-9cb0-e327103d0a4e" {
		t.Fatalf("SessionID = %q", contrib.SessionID)
	}
	if contrib.PreferredRequestID != "req-claude-1" {
		t.Fatalf("PreferredRequestID = %q", contrib.PreferredRequestID)
	}
	if contrib.ConversationID != "b3e8f88e-3fe8-421f-9cb0-e327103d0a4e" {
		t.Fatalf("ConversationID = %q", contrib.ConversationID)
	}
	if contrib.ConversationSource != conversationSourceHeader {
		t.Fatalf("ConversationSource = %q", contrib.ConversationSource)
	}
}

func TestExtractIdentityClaudeAbsentHeaders(t *testing.T) {
	contrib := extractIdentity(http.Header{})
	if contrib.SessionID != "" || contrib.PreferredRequestID != "" || contrib.ConversationID != "" {
		t.Fatalf("expected zero contribution, got %+v", contrib)
	}
}

func TestExtractIdentityClaudeBlankSession(t *testing.T) {
	headers := http.Header{
		"X-Claude-Code-Session-Id": []string{"   "},
		"X-Client-Request-Id":      []string{"req-claude-1"},
	}
	contrib := extractIdentity(headers)
	if contrib.SessionID != "" || contrib.ConversationID != "" {
		t.Fatalf("blank session should not populate ids, got %+v", contrib)
	}
	if contrib.PreferredRequestID != "req-claude-1" {
		t.Fatalf("PreferredRequestID = %q", contrib.PreferredRequestID)
	}
}

func TestRouteProviderExtractIdentityDelegates(t *testing.T) {
	provider := routeProvider{}
	headers := http.Header{"X-Claude-Code-Session-Id": []string{"sess-1"}}
	contrib := provider.ExtractIdentity(headers)
	if contrib.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q", contrib.SessionID)
	}
}
