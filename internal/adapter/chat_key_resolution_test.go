package adapter

import (
	"net/http"
	"testing"

	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	"goodkind.io/clyde/internal/clydeingress"
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
	corr := clydeingress.FromHTTPHeader(header, "req-1")
	ingress := adaptercursor.NewIngress()
	ingressCtx := ingress.TranslateHeaders(header)
	corr = clydeingress.WithChatIdentity(corr, ingressCtx.ConversationID, "native", ingressCtx.ConversationID, "")
	if got := clydeingress.ChatKey(corr); got != "header-conv" {
		t.Fatalf("header should resolve ChatKey, got %q", got)
	}
	corr = clydeingress.WithChatKey(corr, "body-conv")
	if got := clydeingress.ChatKey(corr); got != "header-conv" {
		t.Fatalf("body backfill must not overwrite header value, got %q", got)
	}
}

// TestChatKeyBodyBackfillWhenHeaderMissing confirms the post-TranslateRequest
// path picks up the chat key from the parsed cursor metadata when no header
// was set.
func TestChatKeyBodyBackfillWhenHeaderMissing(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	corr := clydeingress.FromHTTPHeader(header, "req-2")
	if got := clydeingress.ChatKey(corr); got != "" {
		t.Fatalf("ChatKey should be empty before backfill, got %q", got)
	}
	corr = clydeingress.WithChatKey(corr, "body-conv")
	if got := clydeingress.ChatKey(corr); got != "body-conv" {
		t.Fatalf("backfill should populate ChatKey, got %q", got)
	}
}

// TestChatKeyClaudeCodeSessionFallback covers the native Anthropic ingress
// path: only the x-claude-code-session-id header is set.
func TestChatKeyClaudeCodeSessionFallback(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set(clydeingress.HeaderClaudeCodeSessionID, "claude-sess")
	corr := clydeingress.FromHTTPHeader(header, "req-3")
	if got := clydeingress.ChatKey(corr); got != "claude-sess" {
		t.Fatalf("expected claude session id, got %q", got)
	}
}

// TestChatKeyUnresolvedRemainsEmpty confirms that without any signal the
// router will see an empty key and skip the per-chat tee.
func TestChatKeyUnresolvedRemainsEmpty(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	corr := clydeingress.FromHTTPHeader(header, "req-4")
	corr = clydeingress.WithChatKey(corr, "")
	if got := clydeingress.ChatKey(corr); got != "" {
		t.Fatalf("unresolved ChatKey should remain empty, got %q", got)
	}
}
