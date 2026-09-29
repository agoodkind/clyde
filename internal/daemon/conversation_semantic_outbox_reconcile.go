package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// embeddedOutboxStateAborted marks a batch or projection that owner
// reconciliation retired. A replay skips it.
const embeddedOutboxStateAborted = "aborted"

// embeddedUnknownFieldDigest is the committed_fields digest of a field that
// reconciliation found published in the library while the outbox had lost its
// digest. SelectNewFields treats the key as committed and sends no row for it.
// A later pass that projects the field counts it as changed_committed, because
// no projected digest equals this marker.
const embeddedUnknownFieldDigest = "reconciled:unknown-digest"

// embeddedReconciledField is one committed field that reconciliation rebuilt
// from the published row keys.
type embeddedReconciledField struct {
	FieldKey        string
	GenerationOrder uint64
}

// embeddedUnfinishedBatch is one pending or blocked batch of an owner.
type embeddedUnfinishedBatch struct {
	BatchID         string
	GenerationOrder uint64
	Blocked         bool
}

// unfinishedBatches returns the pending and blocked batches of one owner.
func (outbox *conversationSemanticOutbox) unfinishedBatches(ctx context.Context, namespace string, ownerID string) (batches []embeddedUnfinishedBatch, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_unfinished_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", ownerID,
				"err", err,
			)
		}
	}()
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT batch_id, generation_order, state FROM batches
		WHERE namespace = ? AND owner_id = ? AND state IN (?, ?) ORDER BY generation_order`,
		namespace, ownerID, embeddedOutboxStatePending, embeddedOutboxStateBlocked,
	)
	if err != nil {
		return nil, fmt.Errorf("query unfinished batches of %s: %w", ownerID, err)
	}
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	for rows.Next() {
		var batch embeddedUnfinishedBatch
		var state string
		if err := rows.Scan(&batch.BatchID, &batch.GenerationOrder, &state); err != nil {
			return nil, fmt.Errorf("scan unfinished batch of %s: %w", ownerID, err)
		}
		batch.Blocked = state == embeddedOutboxStateBlocked
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read unfinished batches of %s: %w", ownerID, err)
	}
	return batches, nil
}

// rebuildOwner replaces the committed fields of one owner with fields, records
// metadata as stale owner metadata at the library projection order, marks the
// aborted batches and every pending or blocked projection of the owner
// aborted, marks every other blocked batch of the owner aborted, and deletes
// the stored rows of every aborted batch of the owner, in one transaction.
func (outbox *conversationSemanticOutbox) rebuildOwner(
	ctx context.Context,
	namespace string,
	ownerID string,
	fields []embeddedReconciledField,
	metadata embeddedOwnerMetadata,
	projectionOrder uint64,
	abortedBatchIDs []string,
) error {
	err := outbox.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM committed_fields WHERE namespace = ? AND owner_id = ?`, namespace, ownerID); err != nil {
			return fmt.Errorf("clear committed fields: %w", err)
		}
		for _, field := range fields {
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO committed_fields (namespace, owner_id, field_key, digest, provider_message_id, committed_generation) VALUES (?, ?, ?, ?, '', ?)`,
				namespace, ownerID, field.FieldKey, embeddedUnknownFieldDigest, field.GenerationOrder,
			); err != nil {
				return fmt.Errorf("record committed field %s: %w", field.FieldKey, err)
			}
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO owner_metadata (namespace, owner_id, provider, workspace_root, archived, subagent, projection_order, stale)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT (namespace, owner_id) DO UPDATE SET provider = excluded.provider, workspace_root = excluded.workspace_root,
			archived = excluded.archived, subagent = excluded.subagent, projection_order = excluded.projection_order, stale = 1`,
			namespace, ownerID, metadata.Provider, metadata.WorkspaceRoot, metadata.Archived, metadata.Subagent, projectionOrder,
		); err != nil {
			return fmt.Errorf("record owner metadata: %w", err)
		}
		for _, batchID := range abortedBatchIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE batches SET state = ? WHERE batch_id = ?`, embeddedOutboxStateAborted, batchID); err != nil {
				return fmt.Errorf("mark batch %s aborted: %w", batchID, err)
			}
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE batches SET state = ? WHERE namespace = ? AND owner_id = ? AND state = ?`,
			embeddedOutboxStateAborted, namespace, ownerID, embeddedOutboxStateBlocked,
		); err != nil {
			return fmt.Errorf("clear blocked batches: %w", err)
		}
		// An aborted batch keeps no stored text.
		if _, err := tx.ExecContext(
			ctx,
			`DELETE FROM batch_rows WHERE batch_id IN (SELECT batch_id FROM batches WHERE namespace = ? AND owner_id = ? AND state = ?)`,
			namespace, ownerID, embeddedOutboxStateAborted,
		); err != nil {
			return fmt.Errorf("delete aborted batch rows: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE projections SET state = ? WHERE namespace = ? AND owner_id = ? AND state IN (?, ?)`,
			embeddedOutboxStateAborted, namespace, ownerID, embeddedProjectionStatePending, embeddedOutboxStateBlocked,
		); err != nil {
			return fmt.Errorf("clear pending and blocked projections: %w", err)
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.rebuild_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", ownerID,
			"err", err,
		)
		return fmt.Errorf("rebuild outbox state of %s: %w", ownerID, err)
	}
	return nil
}
