package daemon

import (
	"log/slog"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	zedparser "goodkind.io/clyde/internal/providers/zed/parser"
)

// newConversationRegistry builds the conversation parser registry the daemon
// injects into its index. This is the one place the provider parser packages are
// wired in; the conversation package itself imports no provider package, so the
// only coupling between the index and the providers is this registration.
func newConversationRegistry() *conversation.Registry {
	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	registry.Register(codexparser.New())
	registry.Register(cursorparser.New())
	registry.Register(zedparser.New())
	return registry
}

// NewConversationIndex builds a disk-backed conversation index with the Claude,
// Codex, Cursor, and Zed parsers wired in. It serves callers outside the daemon
// worker, such as the scalar-backfill CLI command, that need the same derived
// records the daemon serves without standing up the full daemon. It reads the
// global config so those callers hide subagent conversations exactly as the
// daemon does; an unreadable config falls back to the defaults.
func NewConversationIndex() *conversation.Index {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.Warn("daemon.conversation_index.config_load_failed", "concern", "conversation.index", "component", "daemon", "path", config.GlobalConfigPath(), "err", err)
		return conversation.NewIndex(newConversationRegistry(), config.NewConfigWithDefaults().Conversation)
	}
	return conversation.NewIndex(newConversationRegistry(), cfg.Conversation)
}
