package mitm

import "testing"

func TestCaptureConversationFieldsDerivesClydeID(t *testing.T) {
	t.Parallel()

	conversationID, source := captureConversationFields("claude", IdentityContribution{
		ConversationID:     "sess-native",
		ConversationSource: "header",
	})
	if conversationID != "claude:sess-native" {
		t.Fatalf("conversationID = %q", conversationID)
	}
	if source != "header" {
		t.Fatalf("source = %q", source)
	}
}

func TestCaptureConversationFieldsDefaultsSource(t *testing.T) {
	t.Parallel()

	_, source := captureConversationFields("codex", IdentityContribution{
		ConversationID: "thread-1",
	})
	if source != "header" {
		t.Fatalf("source = %q", source)
	}
}

func TestCaptureConversationFieldsEmptyNativeID(t *testing.T) {
	t.Parallel()

	conversationID, source := captureConversationFields("claude", IdentityContribution{})
	if conversationID != "" || source != "" {
		t.Fatalf("got conversationID=%q source=%q", conversationID, source)
	}
}

func TestCaptureConversationFieldsUnknownProvider(t *testing.T) {
	t.Parallel()

	conversationID, source := captureConversationFields("unspecified", IdentityContribution{
		ConversationID: "native",
	})
	if conversationID != "" || source != "" {
		t.Fatalf("got conversationID=%q source=%q", conversationID, source)
	}
}
