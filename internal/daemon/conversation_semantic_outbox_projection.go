package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/clock"
)

// Outbox projection states.
const (
	embeddedProjectionStatePending = "pending"
	embeddedProjectionStateApplied = "applied"
)

// ownerMetadata returns the recorded metadata of every owner of namespace
// with committed occurrences.
func (outbox *conversationSemanticOutbox) ownerMetadata(ctx context.Context, namespace string) (stored map[string]embeddedStoredOwnerMetadata, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_owner_metadata_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"path", outbox.path,
				"err", err,
			)
		}
	}()
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT owner_id, provider, workspace_root, archived, subagent, projection_order, stale FROM owner_metadata WHERE namespace = ?`,
		namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("query owner metadata: %w", err)
	}
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	stored = make(map[string]embeddedStoredOwnerMetadata)
	for rows.Next() {
		var ownerID string
		var record embeddedStoredOwnerMetadata
		if err := rows.Scan(
			&ownerID, &record.Metadata.Provider, &record.Metadata.WorkspaceRoot, &record.Metadata.Archived,
			&record.Metadata.Subagent, &record.ProjectionOrder, &record.Stale,
		); err != nil {
			return nil, fmt.Errorf("scan owner metadata: %w", err)
		}
		stored[ownerID] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read owner metadata: %w", err)
	}
	return stored, nil
}

// committedRowKeys returns the row keys of every delivered batch of one owner
// in row key order.
func (outbox *conversationSemanticOutbox) committedRowKeys(ctx context.Context, namespace string, ownerID string) ([]string, error) {
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT DISTINCT batch_rows.row_key FROM batch_rows JOIN batches ON batches.batch_id = batch_rows.batch_id
		WHERE batches.namespace = ? AND batches.owner_id = ? AND batches.state = ? ORDER BY batch_rows.row_key`,
		namespace, ownerID, embeddedOutboxStateDelivered,
	)
	var rowKeys []string
	if err == nil {
		rowKeys, err = scanOutboxRowKeys(rows)
	}
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_row_keys_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", ownerID,
			"err", err,
		)
		return nil, fmt.Errorf("read committed row keys of %s: %w", ownerID, err)
	}
	return rowKeys, nil
}

// projectionRowKeys returns the row keys that one recorded projection covers
// in row key order.
func (outbox *conversationSemanticOutbox) projectionRowKeys(ctx context.Context, projection embeddedOutboxProjection) ([]string, error) {
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT row_key FROM projection_rows WHERE namespace = ? AND owner_id = ? AND projection_order = ? ORDER BY row_key`,
		projection.Namespace, projection.OwnerID, projection.ProjectionOrder,
	)
	var rowKeys []string
	if err == nil {
		rowKeys, err = scanOutboxRowKeys(rows)
	}
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_projection_rows_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", projection.OwnerID,
			"projection_order", projection.ProjectionOrder,
			"err", err,
		)
		return nil, fmt.Errorf("read projection %d row keys of %s: %w", projection.ProjectionOrder, projection.OwnerID, err)
	}
	return rowKeys, nil
}

// scanOutboxRowKeys reads one row key column and closes rows.
func scanOutboxRowKeys(rows *sql.Rows) (rowKeys []string, err error) {
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	for rows.Next() {
		var rowKey string
		if err := rows.Scan(&rowKey); err != nil {
			slog.Warn("daemon.conversation_semantic_outbox.scan_row_key_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"err", err,
			)
			return nil, fmt.Errorf("scan row key: %w", err)
		}
		rowKeys = append(rowKeys, rowKey)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read row keys: %w", err)
	}
	return rowKeys, nil
}

// recordProjection saves one pending projection and its row keys in one
// transaction.
func (outbox *conversationSemanticOutbox) recordProjection(ctx context.Context, projection embeddedOutboxProjection, rowKeys []string) error {
	err := outbox.write(ctx, func(tx *sql.Tx) error {
		metadata := projection.Metadata
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO projections (namespace, owner_id, projection_order, token, provider, workspace_root, archived, subagent,
			state, receipt_fingerprint, created_unix) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?)`,
			projection.Namespace, projection.OwnerID, projection.ProjectionOrder, projection.Token, metadata.Provider,
			metadata.WorkspaceRoot, metadata.Archived, metadata.Subagent, embeddedProjectionStatePending, clock.Now().Unix(),
		); err != nil {
			return fmt.Errorf("save projection: %w", err)
		}
		for _, rowKey := range rowKeys {
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO projection_rows (namespace, owner_id, projection_order, row_key) VALUES (?, ?, ?, ?)`,
				projection.Namespace, projection.OwnerID, projection.ProjectionOrder, rowKey,
			); err != nil {
				return fmt.Errorf("save projection row %s: %w", rowKey, err)
			}
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.record_projection_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", projection.OwnerID,
			"projection_order", projection.ProjectionOrder,
			"err", err,
		)
		return fmt.Errorf("record projection %d of %s: %w", projection.ProjectionOrder, projection.OwnerID, err)
	}
	return nil
}

// pendingProjections returns every pending projection in the order it was
// recorded.
func (outbox *conversationSemanticOutbox) pendingProjections(ctx context.Context) (projections []embeddedOutboxProjection, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_pending_projections_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"path", outbox.path,
				"err", err,
			)
		}
	}()
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT namespace, owner_id, projection_order, token, provider, workspace_root, archived, subagent
		FROM projections WHERE state = ? ORDER BY created_unix, owner_id, projection_order`,
		embeddedProjectionStatePending,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending projections: %w", err)
	}
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	for rows.Next() {
		var projection embeddedOutboxProjection
		if err := rows.Scan(
			&projection.Namespace, &projection.OwnerID, &projection.ProjectionOrder, &projection.Token, &projection.Metadata.Provider,
			&projection.Metadata.WorkspaceRoot, &projection.Metadata.Archived, &projection.Metadata.Subagent,
		); err != nil {
			return nil, fmt.Errorf("scan pending projection: %w", err)
		}
		projections = append(projections, projection)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending projections: %w", err)
	}
	return projections, nil
}

// acknowledgeProjection marks one pending projection applied with its
// receipt fingerprint and records its metadata and order as the owner
// metadata, in one transaction.
func (outbox *conversationSemanticOutbox) acknowledgeProjection(ctx context.Context, projection embeddedOutboxProjection, receiptFingerprint string) error {
	err := outbox.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(
			ctx,
			`UPDATE projections SET state = ?, receipt_fingerprint = ? WHERE namespace = ? AND owner_id = ? AND projection_order = ? AND state = ?`,
			embeddedProjectionStateApplied, receiptFingerprint, projection.Namespace, projection.OwnerID, projection.ProjectionOrder,
			embeddedProjectionStatePending,
		)
		if err != nil {
			return fmt.Errorf("mark projection applied: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count applied projections: %w", err)
		}
		if updated != 1 {
			return errors.New("the outbox has no pending projection with that order")
		}
		metadata := projection.Metadata
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE owner_metadata SET provider = ?, workspace_root = ?, archived = ?, subagent = ?, projection_order = ?, stale = 0
			WHERE namespace = ? AND owner_id = ?`,
			metadata.Provider, metadata.WorkspaceRoot, metadata.Archived, metadata.Subagent, projection.ProjectionOrder,
			projection.Namespace, projection.OwnerID,
		); err != nil {
			return fmt.Errorf("record owner metadata: %w", err)
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.acknowledge_projection_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", projection.OwnerID,
			"projection_order", projection.ProjectionOrder,
			"err", err,
		)
		return fmt.Errorf("acknowledge projection %d of %s: %w", projection.ProjectionOrder, projection.OwnerID, err)
	}
	return nil
}
