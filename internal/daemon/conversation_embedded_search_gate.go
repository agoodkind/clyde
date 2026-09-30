package daemon

import (
	"context"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

// search serializes source access with worker shutdown and reconciliation.
// Runtime search-only ownership is added before enabling embedded search.
func (gate *embeddedReconcileGate) search(ctx context.Context, semantic config.ConversationSemanticConfig, options conversation.SearchConversationsOptions, index *conversation.Index) (conversation.SearchConversationsResult, error) {
	gate.mu.Lock()
	embedded, log := gate.embedded, gate.log
	gate.mu.Unlock()
	if embedded == nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(nil)
	}
	embedded.mu.Lock()
	defer embedded.mu.Unlock()
	store, err := embedded.ensureStore(ctx, log)
	if err != nil {
		return conversation.SearchConversationsResult{}, embeddedSearchCallError(ctx, err)
	}
	source := embeddedConversationSearchSource{library: store.library, semantic: semantic, gate: nil, index: index, outbox: store.outbox}
	return source.SearchConversations(ctx, options)
}
