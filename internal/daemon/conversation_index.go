package daemon

import (
	"log/slog"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	copilotparser "goodkind.io/clyde/internal/providers/copilot/parser"
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
	registry.Register(copilotparser.New())
	registry.Register(cursorparser.New())
	registry.Register(zedparser.New())
	return registry
}

// ConversationProviders returns the providers wired into the conversation
// parser registry in display order.
func ConversationProviders() []providerid.Provider {
	providers := newConversationRegistry().Providers()
	slices.SortFunc(providers, func(left, right providerid.Provider) int {
		return strings.Compare(left.String(), right.String())
	})
	return providers
}

// NewConversationIndex builds a disk-backed conversation index with every
// registered parser wired in. It serves callers outside the daemon worker, such
// as the scalar-backfill CLI command, that need the same derived records the
// daemon serves without standing up the full daemon. It reads the global config
// so those callers hide subagent conversations exactly as the daemon does; an
// unreadable config falls back to the defaults.
func NewConversationIndex() *conversation.Index {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.Warn("daemon.conversation_index.config_load_failed", "concern", "conversation.index", "component", "daemon", "path", config.GlobalConfigPath(), "err", err)
		return conversation.NewIndex(newConversationRegistry(), config.NewConfigWithDefaults().Conversation)
	}
	return conversation.NewIndex(newConversationRegistry(), cfg.Conversation)
}
