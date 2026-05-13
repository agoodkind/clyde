package session_test

import (
	"testing"

	"goodkind.io/clyde/internal/session"
)

func TestSessionProviderTitleUsesRegisteredResolver(t *testing.T) {
	t.Cleanup(func() {
		session.RegisterCurrentTitleResolver(nil)
	})
	session.RegisterCurrentTitleResolver(func(provider session.ProviderID, transcriptPath string) (string, error) {
		if provider != session.ProviderClaude {
			t.Fatalf("provider = %q, want claude", provider)
		}
		if transcriptPath != "/tmp/session.jsonl" {
			t.Fatalf("transcriptPath = %q, want /tmp/session.jsonl", transcriptPath)
		}
		return "  Provider Title  ", nil
	})

	sess := session.NewSession("Clyde Title", "session-123")
	sess.Metadata.TranscriptPath = "/tmp/session.jsonl"

	got := sess.ProviderTitle()
	if got != "Provider Title" {
		t.Fatalf("ProviderTitle() = %q, want Provider Title", got)
	}
}
