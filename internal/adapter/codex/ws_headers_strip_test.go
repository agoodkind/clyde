package codex

import "testing"

// TestBuildResponsesWebsocketHeadersStripsConfiguredWireFlags asserts the
// configured strip list drops a named token from the comma-joined
// X-Codex-Beta-Features header while leaving the others in order. The list is
// operator config, so the test uses generic fixture tokens, not any real codex
// capability vocabulary.
func TestBuildResponsesWebsocketHeadersStripsConfiguredWireFlags(t *testing.T) {
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:      "req-strip",
		ConversationID: "conv-strip",
		Token:          "tok",
		TurnState:      NewTurnState(),
		BetaFeatures:   "keep-a,strip-me,keep-b",
		StripWireFlags: []string{"strip-me"},
	})
	if got := header.Get(CodexBetaFeaturesHeader); got != "keep-a,keep-b" {
		t.Fatalf("%s=%q want %q", CodexBetaFeaturesHeader, got, "keep-a,keep-b")
	}
}

// TestBuildResponsesWebsocketHeadersEmptyStripListIsNoOp confirms the default
// (empty strip list) leaves the capability header untouched.
func TestBuildResponsesWebsocketHeadersEmptyStripListIsNoOp(t *testing.T) {
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:      "req-noop",
		ConversationID: "conv-noop",
		Token:          "tok",
		TurnState:      NewTurnState(),
		BetaFeatures:   "keep-a,keep-b",
	})
	if got := header.Get(CodexBetaFeaturesHeader); got != "keep-a,keep-b" {
		t.Fatalf("%s=%q want unchanged %q", CodexBetaFeaturesHeader, got, "keep-a,keep-b")
	}
}
