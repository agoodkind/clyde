package mitmcontrib

import (
	"net/http"
	"testing"
)

func TestExtractIdentityCodexLiveHeaderSet(t *testing.T) {
	session := "019fe7c5-3b02-7140-8bb7-a7e7fadeb1e2"
	headers := http.Header{
		"Session-Id":          []string{session},
		"Thread-Id":           []string{session},
		"X-Client-Request-Id": []string{session},
	}
	contrib := extractIdentity(headers)
	if contrib.SessionID != session {
		t.Fatalf("SessionID = %q", contrib.SessionID)
	}
	if contrib.PreferredRequestID != session {
		t.Fatalf("PreferredRequestID = %q", contrib.PreferredRequestID)
	}
	if contrib.ConversationID != session {
		t.Fatalf("ConversationID = %q", contrib.ConversationID)
	}
	if contrib.ConversationSource != conversationSourceHeader {
		t.Fatalf("ConversationSource = %q", contrib.ConversationSource)
	}
}

func TestExtractIdentityCodexThreadOnly(t *testing.T) {
	headers := http.Header{"Thread-Id": []string{"thread-only"}}
	contrib := extractIdentity(headers)
	if contrib.SessionID != "thread-only" {
		t.Fatalf("SessionID = %q", contrib.SessionID)
	}
	if contrib.ConversationID != "thread-only" {
		t.Fatalf("ConversationID = %q", contrib.ConversationID)
	}
}

func TestExtractIdentityCodexSessionOnly(t *testing.T) {
	headers := http.Header{"Session-Id": []string{"session-only"}}
	contrib := extractIdentity(headers)
	if contrib.SessionID != "session-only" {
		t.Fatalf("SessionID = %q", contrib.SessionID)
	}
	if contrib.ConversationID != "session-only" {
		t.Fatalf("ConversationID = %q", contrib.ConversationID)
	}
}

func TestExtractIdentityCodexAbsentHeaders(t *testing.T) {
	contrib := extractIdentity(http.Header{})
	if contrib.SessionID != "" || contrib.ConversationID != "" {
		t.Fatalf("expected zero contribution, got %+v", contrib)
	}
}

func TestRouteProviderExtractIdentityDelegates(t *testing.T) {
	provider := routeProvider{}
	headers := http.Header{"Session-Id": []string{"sess-1"}}
	contrib := provider.ExtractIdentity(headers)
	if contrib.SessionID != "sess-1" {
		t.Fatalf("SessionID = %q", contrib.SessionID)
	}
}
