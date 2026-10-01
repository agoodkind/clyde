package daemon

import (
	"context"
	"fmt"
	"strings"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/conversation"
)

// embeddedReconcileResult reports one owner reconciliation.
type embeddedReconcileResult struct {
	abortedTokens   int
	committedRows   int
	projectionOrder uint64
}

// reconcileOwner rebuilds the outbox state of one owner from the library. It
// aborts the staged rows of every blocked token and of every pending token at
// or below the committed order that the library did not commit, rebuilds the
// committed fields from the published row keys, records the current record
// metadata as stale owner metadata at the library projection order, and clears
// the blocked state of the owner. A pending token above the committed order
// stays pending for replay. The next reprojection applies the record metadata
// to every published row at the library projection order plus one.
func (delivery *embeddedConversationDelivery) reconcileOwner(
	ctx context.Context,
	namespace string,
	record conversation.Record,
	listed library.OwnerOccurrences,
) (embeddedReconcileResult, error) {
	return delivery.reconcileOwnerWithMetadata(ctx, namespace, record, listed, embeddedOwnerMetadataOf(record))
}

func (delivery *embeddedConversationDelivery) reconcileOwnerWithMetadata(ctx context.Context, namespace string, record conversation.Record, listed library.OwnerOccurrences, metadata embeddedOwnerMetadata) (embeddedReconcileResult, error) {
	var result embeddedReconcileResult
	unfinished, err := delivery.outbox.unfinishedBatches(ctx, namespace, record.ID)
	if err != nil {
		return result, err
	}
	committed := listed.State
	aborted := make([]string, 0, len(unfinished))
	for _, batch := range unfinished {
		if !batch.Blocked && batch.GenerationOrder > committed.GenerationOrder {
			continue
		}
		aborted = append(aborted, batch.BatchID)
		if batch.GenerationOrder == committed.GenerationOrder && batch.BatchID == committed.IdempotencyToken {
			continue
		}
		key := library.GenerationKey{
			Namespace:        namespace,
			OwnerID:          record.ID,
			GenerationOrder:  batch.GenerationOrder,
			IdempotencyToken: batch.BatchID,
		}
		if err := delivery.library.AbortGeneration(ctx, key); err != nil {
			delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.abort_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", record.ID,
				"generation_order", batch.GenerationOrder,
				"token", batch.BatchID,
				"err", err,
			)
			return result, fmt.Errorf("abort generation %d of %s: %w", batch.GenerationOrder, record.ID, err)
		}
		result.abortedTokens++
		delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.generation_aborted",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", record.ID,
			"generation_order", batch.GenerationOrder,
			"token", batch.BatchID,
			"blocked", batch.Blocked,
		)
	}
	fields := embeddedReconciledFields(listed.Rows)
	if err := delivery.outbox.rebuildOwner(ctx, namespace, record.ID, fields, metadata, listed.ProjectionOrder, aborted); err != nil {
		return result, err
	}
	result.committedRows = len(listed.Rows)
	result.projectionOrder = listed.ProjectionOrder
	delivery.log.InfoContext(ctx, "daemon.conversation_semantic_embedded.owner_reconciled",
		"concern", "conversation.semantic",
		"component", "daemon",
		"conversation_id", record.ID,
		"row_count", result.committedRows,
		"field_count", len(fields),
		"projection_order", result.projectionOrder,
		"generation_order", committed.GenerationOrder,
		"aborted_tokens", result.abortedTokens,
	)
	return result, nil
}

// embeddedReconciledFields maps published row keys to field keys by removing
// the part suffix after the last slash. Each field keeps the highest
// generation order among its parts.
func embeddedReconciledFields(rows []library.OwnerOccurrence) []embeddedReconciledField {
	fields := make([]embeddedReconciledField, 0, len(rows))
	positions := make(map[string]int, len(rows))
	for _, row := range rows {
		separator := strings.LastIndex(row.RowKey, "/")
		if separator <= 0 {
			continue
		}
		fieldKey := row.RowKey[:separator]
		position, seen := positions[fieldKey]
		if !seen {
			positions[fieldKey] = len(fields)
			fields = append(fields, embeddedReconciledField{FieldKey: fieldKey, GenerationOrder: row.GenerationOrder})
			continue
		}
		if row.GenerationOrder > fields[position].GenerationOrder {
			fields[position].GenerationOrder = row.GenerationOrder
		}
	}
	return fields
}

// reconcileEmbeddedOwners reconciles every listed owner that has a blocked
// outbox item, and every listed owner that the outbox has no owner metadata
// for while the library reports a committed generation or published rows. An
// owner that the library does not hold is remembered for the life of the
// worker. It returns the reconciled owners.
func (w *conversationSemanticSyncWorker) reconcileEmbeddedOwners(
	ctx context.Context,
	store *embeddedConversationStore,
	stampedRecords []conversation.StampedRecord,
	blockedInOutbox map[string]bool,
	stats *embeddedSyncStats,
) map[string]bool {
	reconciled := make(map[string]bool)
	known, err := w.embeddedAliasOwnerMetadata(ctx, store)
	if err != nil {
		stats.reconcileFailed++
		return reconciled
	}
	seen := make(map[string]bool, len(stampedRecords))
	for _, stampedRecord := range stampedRecords {
		record := stampedRecord.Record
		if record.ID == "" || seen[record.ID] || semanticSyncContextDone(ctx) {
			continue
		}
		seen[record.ID] = true
		_, isKnown := known[record.ID]
		blocked := blockedInOutbox[record.ID]
		if !blocked && (isKnown || w.embedded.libraryAbsent[record.ID]) {
			continue
		}
		listed, err := store.library.ListOwnerOccurrences(ctx, store.namespace.ID, record.ID)
		if err != nil {
			w.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.owner_occurrences_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", record.ID,
				"err", err,
			)
			stats.reconcileFailed++
			continue
		}
		if !blocked && listed.State.GenerationOrder == 0 && len(listed.Rows) == 0 {
			w.embedded.libraryAbsent[record.ID] = true
			continue
		}
		metadata := embeddedOwnerMetadataOf(record)
		if decision := w.embedded.aliasDecision; decision != nil && decision.source.Record.ID == record.ID {
			metadata = decision.metadata
		}
		result, err := store.delivery.reconcileOwnerWithMetadata(ctx, store.namespace.ID, record, listed, metadata)
		if err != nil {
			stats.reconcileFailed++
			continue
		}
		reconciled[record.ID] = true
		known[record.ID] = embeddedStoredOwnerMetadata{Metadata: metadata, ProjectionOrder: listed.ProjectionOrder, Stale: true}
		stats.reconciledOwners++
		stats.abortedTokens += result.abortedTokens
	}
	return reconciled
}
