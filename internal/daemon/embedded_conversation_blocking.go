package daemon

import (
	"context"
	"errors"

	"goodkind.io/lm-semantic-search/library"
)

// permanentLibraryErrorClass classifies a library error that no replay can
// resolve. [library.ErrStaleGeneration] and [library.ErrAppendConflict] are
// permanent. Every other error is transient, and the item stays pending.
func permanentLibraryErrorClass(err error) (embeddedBlockedClass, bool) {
	switch {
	case errors.Is(err, library.ErrStaleGeneration):
		return embeddedBlockedStaleGeneration, true
	case errors.Is(err, library.ErrAppendConflict):
		return embeddedBlockedAppendConflict, true
	default:
		return "", false
	}
}

// blockBatchOnPermanentError moves batch to the blocked state when err is
// permanent and logs one owner_blocked record for the transition.
func (delivery *embeddedConversationDelivery) blockBatchOnPermanentError(ctx context.Context, batch embeddedOutboxBatch, err error) {
	class, permanent := permanentLibraryErrorClass(err)
	if !permanent {
		return
	}
	libraryOrder := delivery.libraryOwnerOrder(ctx, batch.Namespace, batch.OwnerID)
	transitioned, blockErr := delivery.outbox.blockBatch(ctx, batch, class, libraryOrder)
	if blockErr != nil || !transitioned {
		return
	}
	delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.owner_blocked",
		"concern", "conversation.semantic",
		"component", "daemon",
		"conversation_id", batch.OwnerID,
		"batch_id", batch.BatchID,
		"error_class", string(class),
		"batch_order", batch.GenerationOrder,
		"library_order", libraryOrder,
		"err", err,
	)
}

// blockProjectionOnPermanentError moves projection to the blocked state when
// err is permanent and logs one owner_blocked record for the transition.
func (delivery *embeddedConversationDelivery) blockProjectionOnPermanentError(
	ctx context.Context,
	projection embeddedOutboxProjection,
	err error,
) {
	class, permanent := permanentLibraryErrorClass(err)
	if !permanent {
		return
	}
	libraryOrder := delivery.libraryProjectionOrder(ctx, projection.Namespace, projection.OwnerID)
	transitioned, blockErr := delivery.outbox.blockProjection(ctx, projection, class, libraryOrder)
	if blockErr != nil || !transitioned {
		return
	}
	delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.owner_blocked",
		"concern", "conversation.semantic",
		"component", "daemon",
		"conversation_id", projection.OwnerID,
		"projection_order", projection.ProjectionOrder,
		"error_class", string(class),
		"library_order", libraryOrder,
		"err", err,
	)
}

// libraryProjectionOrder returns the highest saved ReprojectScalars order of
// one owner from ListOwnerOccurrences. A failed read logs the error and
// returns zero.
func (delivery *embeddedConversationDelivery) libraryProjectionOrder(ctx context.Context, namespace string, ownerID string) uint64 {
	listed, err := delivery.library.ListOwnerOccurrences(ctx, namespace, ownerID)
	if err != nil {
		delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.owner_occurrences_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", ownerID,
			"err", err,
		)
		return 0
	}
	return listed.ProjectionOrder
}

// libraryOwnerOrder returns the committed generation order of one owner. A
// failed read logs the error and returns zero.
func (delivery *embeddedConversationDelivery) libraryOwnerOrder(ctx context.Context, namespace string, ownerID string) uint64 {
	state, err := delivery.library.GetOwnerState(ctx, namespace, ownerID)
	if err != nil {
		delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.owner_state_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", ownerID,
			"err", err,
		)
		return 0
	}
	return state.GenerationOrder
}
