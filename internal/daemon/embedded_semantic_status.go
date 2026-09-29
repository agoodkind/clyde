package daemon

import (
	"context"
	"sync"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
)

// maxStatusBlockedConversationIDs bounds the blocked conversation IDs that the
// daemon status reports.
const maxStatusBlockedConversationIDs = 10

// embeddedSemanticStatus is the embedded ingestion state that the sync worker
// publishes after each pass and the daemon status RPC reads. The zero state
// reports a closed library with no pending or blocked items.
type embeddedSemanticStatus struct {
	mu                 sync.Mutex
	libraryOpen        bool
	pendingBatches     uint64
	pendingProjections uint64
	blockedOwners      []string
}

func newEmbeddedSemanticStatus() *embeddedSemanticStatus {
	return &embeddedSemanticStatus{
		mu:                 sync.Mutex{},
		libraryOpen:        false,
		pendingBatches:     0,
		pendingProjections: 0,
		blockedOwners:      nil,
	}
}

// publishStore records an open library with the pending and blocked counts
// that the outbox of store reports. A failed outbox read keeps the earlier
// counts.
func (status *embeddedSemanticStatus) publishStore(ctx context.Context, store *embeddedConversationStore) {
	if status == nil || store == nil {
		return
	}
	pendingBatches, pendingProjections, countErr := store.outbox.pendingCounts(ctx)
	blocked, blockedErr := store.outbox.blockedOwners(ctx, store.namespace.ID)
	status.mu.Lock()
	defer status.mu.Unlock()
	status.libraryOpen = true
	if countErr == nil {
		status.pendingBatches = pendingBatches
		status.pendingProjections = pendingProjections
	}
	if blockedErr == nil {
		status.blockedOwners = blocked
	}
}

// markClosed records that the library is closed.
func (status *embeddedSemanticStatus) markClosed() {
	if status == nil {
		return
	}
	status.mu.Lock()
	defer status.mu.Unlock()
	status.libraryOpen = false
}

// proto returns the status message with at most
// maxStatusBlockedConversationIDs blocked conversation IDs.
func (status *embeddedSemanticStatus) proto() *clydev1.EmbeddedSemanticStatus {
	message := &clydev1.EmbeddedSemanticStatus{}
	if status == nil {
		return message
	}
	status.mu.Lock()
	defer status.mu.Unlock()
	message.LibraryOpen = status.libraryOpen
	message.PendingBatches = status.pendingBatches
	message.PendingProjections = status.pendingProjections
	message.BlockedOwners = uint64(len(status.blockedOwners))
	shown := status.blockedOwners[:min(len(status.blockedOwners), maxStatusBlockedConversationIDs)]
	message.BlockedConversationIds = append([]string(nil), shown...)
	return message
}
