package daemon

import (
	"context"

	"goodkind.io/clyde/internal/conversation"
)

// conversationSearchSource is the single lookup boundary used by the control
// server. Implementations own their retrieval mechanics and return domain
// matches or a typed conversationSearchSourceError.
type conversationSearchSource interface {
	SearchConversations(context.Context, conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error)
}
