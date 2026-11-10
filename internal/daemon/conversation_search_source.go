package daemon

import (
	"context"
	"errors"
	"log/slog"

	"goodkind.io/clyde/internal/config"
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
	backend       config.ConversationSemanticBackend
}

func searchSourceForBackend(backend config.ConversationSemanticBackend) conversation.SearchSource {
	if backend == config.ConversationSemanticBackendLocal {
		return conversation.SearchSourceLocal
	}
	return conversation.SearchSourceSemantic
}

// rawTextFallbackSearchSource answers from primary. It answers from a raw text
// scan when primary is disabled, unavailable, or failed. A refused query
// returns the primary error.
type rawTextFallbackSearchSource struct {
	primary conversationSearchSource
	index   *conversation.Index
}

func (s *rawTextFallbackSearchSource) SearchConversations(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	result, err := s.primary.SearchConversations(ctx, options)
	if err == nil {
		return result, nil
	}
	var sourceFailure conversationSearchSourceError
	if !errors.As(err, &sourceFailure) {
		return conversation.SearchConversationsResult{}, failedConversationSearchSourceError(err)
	}
	switch sourceFailure.code {
	case conversationSearchDisabled, conversationSearchSourceUnavailable, conversationSearchSourceFailed:
	case conversationSearchSourceRefused:
		return conversation.SearchConversationsResult{}, sourceFailure
	default:
		return conversation.SearchConversationsResult{}, sourceFailure
	}
	if options.Cursor != "" {
		return conversation.SearchConversationsResult{}, sourceFailure
	}
	slog.WarnContext(ctx, "daemon.search_conversations.raw_text_fallback",
		"concern", "conversation.search",
		"component", "daemon",
		"failure_code", string(sourceFailure.code),
		"err", sourceFailure.cause,
	)
	rawResult, rawErr := s.index.SearchRawText(ctx, options)
	if rawErr != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.raw_text_failed",
			"concern", "conversation.search",
			"component", "daemon",
			"err", rawErr,
		)
		return conversation.SearchConversationsResult{}, failedConversationSearchSourceError(rawErr)
	}
	return rawResult, nil
}

func (s *semanticConversationSearchSource) SearchConversations(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	if s != nil && s.searchEnabled != nil && !s.searchEnabled() {
		return conversation.SearchConversationsResult{}, disabledConversationSearchSourceError(nil)
	}
	// The engine ranks each request independently and returns no continuation
	// token. This source refuses a cursor instead of reading it as a first page.
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
	return semanticSearchResult(ctx, s.index, client, s.collectionID, searchSourceForBackend(s.backend), options)
}
