package daemon

import (
	"context"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

// search admits concurrent publication and prevents storage closure until it returns.
func (gate *embeddedReconcileGate) search(ctx context.Context, semantic config.ConversationSemanticConfig, options conversation.SearchConversationsOptions, index *conversation.Index) (conversation.SearchConversationsResult, error) {
	gate.mu.Lock()
	embedded, log := gate.embedded, gate.log
	gate.mu.Unlock()
	if embedded == nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError()
	}
	store, err := embedded.readStore(ctx, log)
	if err != nil {
		return conversation.SearchConversationsResult{}, embeddedSearchCallError(ctx, err)
	}
	defer embedded.mu.RUnlock()
	source := embeddedConversationSearchSource{library: store.library, semantic: semantic, gate: nil, index: index, outbox: store.outbox}
	return source.SearchConversations(ctx, options)
}
