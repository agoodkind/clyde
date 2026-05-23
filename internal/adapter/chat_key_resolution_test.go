package adapter

import (
	"net/http"
	"testing"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	"goodkind.io/clyde/internal/correlation"
)

// TestChatKeyHeaderWinsOverBodyBackfill exercises the resolution chain that
// server_dispatch.go runs at ingress: the registered Cursor ingress contract
// extracts x-cursor-conversation-id, and a subsequent WithChatKey call (the
// post-TranslateRequest body backfill) is a no-op when the header path already
// resolved a key.
func TestChatKeyHeaderWinsOverBodyBackfill(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set(adaptercursor.HeaderConversationID, "header-conv")
	corr := correlation.FromHTTPHeader(header, "req-1")
	ingress := adaptercursor.NewIngress()
	ingressCtx := ingress.TranslateHeaders(header)
	corr = corr.WithChatIdentity(ingressCtx.ConversationID, "native", ingressCtx.ConversationID, "")
	if corr.ChatKey != "header-conv" {
		t.Fatalf("header should resolve ChatKey, got %q", corr.ChatKey)
	}
	corr = corr.WithChatKey("body-conv")
	if corr.ChatKey != "header-conv" {
		t.Fatalf("body backfill must not overwrite header value, got %q", corr.ChatKey)
	}
}

// TestChatKeyBodyBackfillWhenHeaderMissing confirms the post-TranslateRequest
// path picks up the chat key from the parsed cursor metadata when no header
// was set.
func TestChatKeyBodyBackfillWhenHeaderMissing(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	corr := correlation.FromHTTPHeader(header, "req-2")
	if corr.ChatKey != "" {
		t.Fatalf("ChatKey should be empty before backfill, got %q", corr.ChatKey)
	}
	corr = corr.WithChatKey("body-conv")
	if corr.ChatKey != "body-conv" {
		t.Fatalf("backfill should populate ChatKey, got %q", corr.ChatKey)
	}
}

// TestChatKeyClaudeCodeSessionFallback covers the native Anthropic ingress
// path: only the x-claude-code-session-id header is set.
func TestChatKeyClaudeCodeSessionFallback(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set(correlation.HeaderClaudeCodeSessionID, "claude-sess")
	corr := correlation.FromHTTPHeader(header, "req-3")
	if corr.ChatKey != "claude-sess" {
		t.Fatalf("expected claude session id, got %q", corr.ChatKey)
	}
}

// TestChatKeyUnresolvedRemainsEmpty confirms that without any signal the
// router will see an empty key and skip the per-chat tee.
func TestChatKeyUnresolvedRemainsEmpty(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	corr := correlation.FromHTTPHeader(header, "req-4")
	corr = corr.WithChatKey("")
	if corr.ChatKey != "" {
		t.Fatalf("unresolved ChatKey should remain empty, got %q", corr.ChatKey)
	}
}
