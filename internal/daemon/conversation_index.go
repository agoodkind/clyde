package daemon

import (
	"goodkind.io/clyde/internal/conversation"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
)

// newConversationRegistry builds the conversation parser registry the daemon
// injects into its index. This is the one place the provider parser packages are
// wired in; the conversation package itself imports no provider package, so the
// only coupling between the index and the providers is this registration.
func newConversationRegistry() *conversation.Registry {
	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	registry.Register(codexparser.New())
	return registry
}
