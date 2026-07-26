package daemon

import (
	"context"
	"log/slog"

	"goodkind.io/clyde/internal/conversation"
)

// conversationSearchSource is the single lookup boundary used by the control
// server. Implementations own their retrieval mechanics and return domain
// matches or a typed conversationSearchSourceError.
type conversationSearchSource interface {
	SearchConversations(context.Context, conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error)
}

// conversationSearchIndex is the cached conversation metadata surface needed
// by the current source to resolve result records and filter scopes.
type conversationSearchIndex interface {
	RecordByID(id string) (conversation.Record, bool)
	ConversationIDsMatching(ctx context.Context, provider conversation.Provider, workspaceRoot string, includeArchived bool) ([]string, error)
}

// semanticConversationSearchSource adapts the vector engine to the generic
// search-source boundary. The client resolves per call so a recovered engine
// connection becomes available without rebuilding the control server.
type semanticConversationSearchSource struct {
	index        conversationSearchIndex
	searchClient func() conversationSemanticSearchClient
	collectionID string
}

func (s *semanticConversationSearchSource) SearchConversations(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	if s == nil || s.searchClient == nil {
		failure := unavailableConversationSearchSourceError(nil)
		slog.WarnContext(ctx, "daemon.search_conversations.source_unavailable",
			"concern", "process.daemon.lifecycle",
			"component", "daemon",
			"source", conversation.SearchSourceSemantic.String(),
			"err", failure,
		)
		return conversation.SearchConversationsResult{}, failure
	}
	client := s.searchClient()
	if client == nil {
		failure := unavailableConversationSearchSourceError(nil)
		slog.WarnContext(ctx, "daemon.search_conversations.source_unavailable",
			"concern", "process.daemon.lifecycle",
			"component", "daemon",
			"source", conversation.SearchSourceSemantic.String(),
			"err", failure,
		)
		return conversation.SearchConversationsResult{}, failure
	}
	return semanticSearchResult(ctx, s.index, client, s.collectionID, options)
}
