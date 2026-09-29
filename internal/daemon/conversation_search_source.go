package daemon

import (
	"context"
	"errors"

	"goodkind.io/clyde/internal/conversation"
)

// errSemanticSourceCursorUnsupported is the refusal cause for a cursor sent to
// the engine-backed source, which pages by offset only.
var errSemanticSourceCursorUnsupported = errors.New("the conversation search source pages by offset and does not accept a cursor")

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
	index         conversationSearchIndex
	searchEnabled func() bool
	searchClient  func() conversationSemanticSearchClient
	collectionID  string
}

func (s *semanticConversationSearchSource) SearchConversations(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	if s != nil && s.searchEnabled != nil && !s.searchEnabled() {
		return conversation.SearchConversationsResult{}, disabledConversationSearchSourceError(nil)
	}
	// The engine ranks each request independently and returns no continuation
	// token. A cursor is refused here instead of being read as a first page.
	if options.Cursor != "" {
		return conversation.SearchConversationsResult{}, refusedConversationSearchSourceError(errSemanticSourceCursorUnsupported)
	}
	if s == nil || s.searchClient == nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(nil)
	}
	client := s.searchClient()
	if client == nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(nil)
	}
	return semanticSearchResult(ctx, s.index, client, s.collectionID, options)
}
