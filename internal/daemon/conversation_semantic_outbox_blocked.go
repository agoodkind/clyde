package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"goodkind.io/clyde/internal/clock"
)

// embeddedOutboxStateBlocked marks a batch or projection that the library
// rejected with a permanent error. A replay skips it, and the pass excludes its
// owner from delivery and reprojection until a reconciliation clears it.
const embeddedOutboxStateBlocked = "blocked"

// embeddedBlockedClass is the library error class that blocked an outbox item.
type embeddedBlockedClass string

const (
	// embeddedBlockedStaleGeneration records [library.ErrStaleGeneration].
	embeddedBlockedStaleGeneration embeddedBlockedClass = "stale_generation"
	// embeddedBlockedAppendConflict records [library.ErrAppendConflict].
	embeddedBlockedAppendConflict embeddedBlockedClass = "append_conflict"
)

// blockBatch moves one pending batch to the blocked state with the error class,
// the library owner order, and the time. It reports whether this call made the
// transition.
func (outbox *conversationSemanticOutbox) blockBatch(
	ctx context.Context,
	batch embeddedOutboxBatch,
	class embeddedBlockedClass,
	libraryOrder uint64,
) (bool, error) {
	transitioned, err := outbox.block(ctx, func(tx *sql.Tx) (sql.Result, error) {
		result, err := tx.ExecContext(ctx,
			`UPDATE batches SET state = ?, blocked_class = ?, blocked_library_order = ?, blocked_unix = ? WHERE batch_id = ? AND state = ?`,
			embeddedOutboxStateBlocked, string(class), libraryOrder, clock.Now().Unix(), batch.BatchID, embeddedOutboxStatePending,
		)
		if err != nil {
			return nil, fmt.Errorf("mark batch blocked: %w", err)
		}
		return result, nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.block_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", batch.OwnerID,
			"batch_id", batch.BatchID,
			"err", err,
		)
		return false, fmt.Errorf("block outbox batch %s of %s: %w", batch.BatchID, batch.OwnerID, err)
	}
	return transitioned, nil
}

// blockProjection moves one pending projection to the blocked state with the
// error class, the library owner order, and the time. It reports whether this
// call made the transition.
func (outbox *conversationSemanticOutbox) blockProjection(
	ctx context.Context,
	projection embeddedOutboxProjection,
	class embeddedBlockedClass,
	libraryOrder uint64,
) (bool, error) {
	transitioned, err := outbox.block(ctx, func(tx *sql.Tx) (sql.Result, error) {
		result, err := tx.ExecContext(ctx,
			`UPDATE projections SET state = ?, blocked_class = ?, blocked_library_order = ?, blocked_unix = ?
			WHERE namespace = ? AND owner_id = ? AND projection_order = ? AND state = ?`,
			embeddedOutboxStateBlocked, string(class), libraryOrder, clock.Now().Unix(),
			projection.Namespace, projection.OwnerID, projection.ProjectionOrder, embeddedProjectionStatePending,
		)
		if err != nil {
			return nil, fmt.Errorf("mark projection blocked: %w", err)
		}
		return result, nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.block_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", projection.OwnerID,
			"projection_order", projection.ProjectionOrder,
			"err", err,
		)
		return false, fmt.Errorf("block outbox projection %d of %s: %w", projection.ProjectionOrder, projection.OwnerID, err)
	}
	return transitioned, nil
}

// block runs one state update in a write transaction and reports whether it
// changed a row.
func (outbox *conversationSemanticOutbox) block(ctx context.Context, update func(*sql.Tx) (sql.Result, error)) (bool, error) {
	transitioned := false
	err := outbox.write(ctx, func(tx *sql.Tx) error {
		result, err := update(tx)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.count_blocked_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"path", outbox.path,
				"err", err,
			)
			return fmt.Errorf("count updated rows: %w", err)
		}
		transitioned = updated == 1
		return nil
	})
	return transitioned, err
}

// blockedOwners returns the sorted owners of namespace with a blocked batch or
// a blocked projection.
func (outbox *conversationSemanticOutbox) blockedOwners(ctx context.Context, namespace string) (owners []string, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_blocked_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"path", outbox.path,
				"err", err,
			)
		}
	}()
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT owner_id FROM batches WHERE namespace = ? AND state = ?
		UNION SELECT owner_id FROM projections WHERE namespace = ? AND state = ?`,
		namespace, embeddedOutboxStateBlocked, namespace, embeddedOutboxStateBlocked,
	)
	if err != nil {
		return nil, fmt.Errorf("query blocked owners: %w", err)
	}
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	for rows.Next() {
		var ownerID string
		if err := rows.Scan(&ownerID); err != nil {
			return nil, fmt.Errorf("scan blocked owner: %w", err)
		}
		owners = append(owners, ownerID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read blocked owners: %w", err)
	}
	sort.Strings(owners)
	return owners, nil
}
