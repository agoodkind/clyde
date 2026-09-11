package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/providerid"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	copilotparser "goodkind.io/clyde/internal/providers/copilot/parser"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	zedparser "goodkind.io/clyde/internal/providers/zed/parser"
)

// startConversationIndex installs lifecycle ownership before launching the worker.
func startConversationIndex(ctx context.Context, log *slog.Logger, index *conversation.Index, group *livetrack.Group) {
	startConversationIndexWorker(ctx, log, group, func(workerCtx context.Context) {
		index.Start(workerCtx, time.Minute)
	})
}

func startConversationIndexWorker(ctx context.Context, log *slog.Logger, group *livetrack.Group, run func(context.Context)) {
	if group == nil {
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	group.AddHookBefore(livetrack.PhaseWorkers, "conversation.index", func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			return fmt.Errorf("wait for conversation index worker: %w", stopCtx.Err())
		}
	})
	go func() {
		defer close(done)
		defer cancel()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.conversation_index.panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		run(workerCtx)
	}()
}

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

// ExportTranscriptLocal resolves and exports one conversation without the
// daemon control socket. CLI callers use this path when the daemon is stopped.
func ExportTranscriptLocal(ctx context.Context, conversationID string, options conversation.ExportOptions) ([]byte, error) {
	index := NewConversationIndex()
	record, err := index.Resolve(ctx, conversationID)
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_export.resolve_failed", "concern", "conversation.export", "component", "daemon", "conversation_id", conversationID, "err", err)
		return nil, fmt.Errorf("resolve conversation: %w", err)
	}
	body, err := index.Export(record, options)
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_export.export_failed", "concern", "conversation.export", "component", "daemon", "conversation_id", conversationID, "err", err)
		return nil, fmt.Errorf("export transcript: %w", err)
	}
	return body, nil
}
