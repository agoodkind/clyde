package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/livetrack"
)

// defaultSemanticContentKinds is what the feeder offers when the config names no
// kinds: the chat turns, and tool invocations without their results.
//
// [conversation.ContentKindToolCalls] is the middle of the three nested tool
// levels, and [conversation.NewContentKindSet] collapses the levels, so naming
// it is what expresses "the call but not what it returned". Reasoning, system
// prompts, system messages, and raw JSON metadata are all absent, so a fresh
// install embeds what a person would search for and nothing else.
func defaultSemanticContentKinds() conversation.ContentKindSet {
	return conversation.NewContentKindSet(
		conversation.ContentKindChat,
		conversation.ContentKindToolCalls,
	)
}

// startConfiguredConversationSemanticSync resolves the configured content kinds
// and starts the feeder under them.
//
// It is called before the control server serves, because the feeder's stop is
// installed on the lifecycle group inside the start call ahead of the goroutine
// launch, so a reload or rebind RPC cannot begin the workers-phase drain while
// the feeder runs unowned.
func startConfiguredConversationSemanticSync(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	index conversationSemanticIndex,
	resolveClient conversationSemanticClientResolver,
	freshness *conversationSemanticFreshness,
	group *livetrack.Group,
) error {
	kinds, err := SemanticContentKinds(cfg.Conversation.Semantic)
	if err != nil {
		// SemanticContentKinds already logged the invalid value and wrapped the
		// error with the config key, so returning it as-is keeps one boundary log
		// per failure rather than two saying the same thing.
		return err
	}
	startConversationSemanticSync(ctx, log, index, resolveClient, cfg.Conversation.Semantic.CollectionID, freshness, group, kinds)
	return nil
}

// SemanticContentKinds resolves the configured selector values into the content
// kinds the feeder offers. It reuses the export surface's selector vocabulary and
// its validation, so a kind is named once for the whole product rather than once
// per consumer, and an unknown name is rejected there with the supported list.
//
// An empty list resolves to [defaultSemanticContentKinds] rather than to nothing.
// The config loader leaves the list as written, and a Config built as a struct
// literal never passes through the loader at all, so reading an unset list as
// "index nothing" would quietly empty the corpus. Turning indexing off is
// `enabled = false`.
func SemanticContentKinds(semantic config.ConversationSemanticConfig) (conversation.ContentKindSet, error) {
	if len(semantic.IndexedContent) == 0 {
		return defaultSemanticContentKinds(), nil
	}
	kinds, err := conversation.ResolveContentKinds(semantic.IndexedContent)
	if err != nil {
		slog.Warn("daemon.conversation_semantic.indexed_content_invalid",
			"concern", "conversation.semantic",
			"component", "daemon",
			"indexed_content", strings.Join(semantic.IndexedContent, ","),
			"err", err,
		)
		return conversation.ContentKindSet{}, fmt.Errorf("resolve conversation.semantic.indexed_content: %w", err)
	}
	return kinds, nil
}
