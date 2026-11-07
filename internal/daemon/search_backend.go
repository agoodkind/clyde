package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

type backendConversationSearchSource struct {
	configured conversationSearchSource
	index      conversationSearchIndex
	settings   func() config.ConversationSemanticConfig
}

func (source *backendConversationSearchSource) SearchConversations(ctx context.Context, options conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error) {
	if options.Backend == "" {
		result, err := source.configured.SearchConversations(ctx, options)
		if err != nil {
			slog.WarnContext(ctx, "daemon.search_conversations.configured_failed", "concern", "conversation.search", "component", "daemon", "err", err)
			return result, fmt.Errorf("search configured backend: %w", err)
		}
		return result, nil
	}
	if err := options.Backend.Validate(); err != nil {
		return conversation.SearchConversationsResult{}, refusedConversationSearchSourceError(err)
	}
	result, err := source.searchBackend(ctx, options)
	if err == nil {
		return result, nil
	}
	var failure conversationSearchSourceError
	if !errors.As(err, &failure) {
		failure = failedConversationSearchSourceError(err)
	}
	failure.backend = options.Backend
	return conversation.SearchConversationsResult{}, failure
}

func (source *backendConversationSearchSource) searchBackend(ctx context.Context, options conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error) {
	settings := source.settings().ForSearchBackend(config.ConversationSemanticBackend(options.Backend))
	if !settings.AnswersSearch() {
		return conversation.SearchConversationsResult{}, disabledConversationSearchSourceError(nil)
	}
	if options.Cursor != "" {
		return conversation.SearchConversationsResult{}, refusedConversationSearchSourceError(errSemanticSourceCursorUnsupported)
	}
	client, err := openRequestedSearchBackend(ctx, settings)
	if err != nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(err)
	}
	defer func() {
		if closeErr := client.Close(context.WithoutCancel(ctx)); closeErr != nil {
			slog.WarnContext(ctx, "daemon.search_conversations.backend_close_failed", "concern", "conversation.search", "component", "daemon", "backend", options.Backend, "err", closeErr)
		}
	}()
	available, err := client.SearchCollectionAvailable(ctx, settings.CollectionID)
	if err != nil || !available {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(err)
	}
	return semanticSearchResult(ctx, source.index, client, settings.CollectionID, searchSourceForBackend(settings.Backend), options)
}

func openRequestedSearchBackend(ctx context.Context, settings config.ConversationSemanticConfig) (*vectorsearch.Client, error) {
	if settings.Backend == config.ConversationSemanticBackendLocal {
		info, err := os.Stat(settings.LocalRoot)
		if err != nil {
			slog.WarnContext(ctx, "daemon.search_conversations.local_index_unavailable", "concern", "conversation.search", "component", "daemon", "path", settings.LocalRoot, "err", err)
			return nil, fmt.Errorf("open existing local index: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("local index %q is not a directory", settings.LocalRoot)
		}
	}
	return OpenConversationSearchClient(ctx, settings)
}
