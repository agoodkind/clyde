package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/conversation"
)

// embeddedOwnerMetadata is the mutable owner metadata that every occurrence
// of one owner stores in the provider, workspace_root, archived, and subagent
// columns.
type embeddedOwnerMetadata struct {
	Provider      string
	WorkspaceRoot string
	Archived      bool
	Subagent      bool
}

// embeddedOwnerMetadataOf reads the mutable owner metadata from one index
// record without loading its transcript.
func embeddedOwnerMetadataOf(record conversation.Record) embeddedOwnerMetadata {
	return embeddedOwnerMetadata{
		Provider:      record.Provider.String(),
		WorkspaceRoot: record.WorkspaceRoot,
		Archived:      record.Archived,
		Subagent:      record.IsSubagent(),
	}
}

// scalars returns the mutable column values of the metadata. An empty
// workspace root is an explicit null.
func (metadata embeddedOwnerMetadata) scalars() map[string]library.ScalarValue {
	return map[string]library.ScalarValue{
		embeddedScalarProvider:      embeddedStringScalar(metadata.Provider),
		embeddedScalarWorkspaceRoot: embeddedOptionalStringScalar(metadata.WorkspaceRoot),
		embeddedScalarArchived:      embeddedBoolScalar(metadata.Archived),
		embeddedScalarSubagent:      embeddedBoolScalar(metadata.Subagent),
	}
}

// embeddedOutboxProjection is one scalar reprojection of one owner. Token is
// the idempotency token.
type embeddedOutboxProjection struct {
	Namespace       string
	OwnerID         string
	ProjectionOrder uint64
	Token           string
	Metadata        embeddedOwnerMetadata
}

// embeddedStoredOwnerMetadata is the outbox record of the metadata that the
// committed occurrences of one owner store. Stale reports that a committed
// generation stored other metadata than the record.
type embeddedStoredOwnerMetadata struct {
	Metadata        embeddedOwnerMetadata
	ProjectionOrder uint64
	Stale           bool
}

// embeddedProjectionToken returns the lowercase hex SHA-256 of the
// length-prefixed namespace, owner ID, decimal projection order, and metadata
// values.
func embeddedProjectionToken(namespace string, ownerID string, projectionOrder uint64, metadata embeddedOwnerMetadata) string {
	hasher := sha256.New()
	values := []string{
		namespace,
		ownerID,
		strconv.FormatUint(projectionOrder, 10),
		metadata.Provider,
		metadata.WorkspaceRoot,
		strconv.FormatBool(metadata.Archived),
		strconv.FormatBool(metadata.Subagent),
	}
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// reproject records one projection of every published row of an owner in the
// outbox and applies it with ReprojectScalars. ListOwnerOccurrences supplies
// the published row keys. The projection order is the recorded order plus
// one.
func (delivery *embeddedConversationDelivery) reproject(
	ctx context.Context,
	namespace string,
	ownerID string,
	stored embeddedStoredOwnerMetadata,
	metadata embeddedOwnerMetadata,
) error {
	listed, err := delivery.library.ListOwnerOccurrences(ctx, namespace, ownerID)
	if err != nil {
		delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.owner_occurrences_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", ownerID,
			"err", err,
		)
		return fmt.Errorf("list published rows of %s: %w", ownerID, err)
	}
	rowKeys := make([]string, 0, len(listed.Rows))
	for _, row := range listed.Rows {
		rowKeys = append(rowKeys, row.RowKey)
	}
	order := stored.ProjectionOrder + 1
	projection := embeddedOutboxProjection{
		Namespace:       namespace,
		OwnerID:         ownerID,
		ProjectionOrder: order,
		Token:           embeddedProjectionToken(namespace, ownerID, order, metadata),
		Metadata:        metadata,
	}
	if err := delivery.outbox.recordProjection(ctx, projection, rowKeys); err != nil {
		return err
	}
	return delivery.applyProjection(ctx, projection, rowKeys)
}

// applyProjection sends one recorded projection to ReprojectScalars and
// acknowledges the receipt. ReprojectScalars returns the saved receipt for a
// token that the library already applied. A permanent library error moves the
// projection to the blocked state.
func (delivery *embeddedConversationDelivery) applyProjection(
	ctx context.Context,
	projection embeddedOutboxProjection,
	rowKeys []string,
) error {
	values := projection.Metadata.scalars()
	rows := make(map[string]map[string]library.ScalarValue, len(rowKeys))
	for _, rowKey := range rowKeys {
		rows[rowKey] = values
	}
	receipt, err := delivery.library.ReprojectScalars(ctx, library.ScalarProjection{
		Namespace:        projection.Namespace,
		OwnerID:          projection.OwnerID,
		ProjectionOrder:  projection.ProjectionOrder,
		IdempotencyToken: projection.Token,
		Rows:             rows,
	})
	if err == nil && receipt.ProjectionOrder != projection.ProjectionOrder {
		err = fmt.Errorf("receipt order %d differs from projection order %d", receipt.ProjectionOrder, projection.ProjectionOrder)
	}
	if err != nil {
		delivery.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.reproject_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", projection.OwnerID,
			"projection_order", projection.ProjectionOrder,
			"rows", len(rowKeys),
			"err", err,
		)
		delivery.blockProjectionOnPermanentError(ctx, projection, err)
		return fmt.Errorf("reproject %d rows of %s at order %d: %w", len(rowKeys), projection.OwnerID, projection.ProjectionOrder, err)
	}
	return delivery.outbox.acknowledgeProjection(ctx, projection, receipt.Fingerprint)
}

// replayPendingProjections applies every pending outbox projection with its
// stored row keys. It returns the count of acknowledged projections and the
// owners with a projection that stays pending.
func (delivery *embeddedConversationDelivery) replayPendingProjections(ctx context.Context) (int, map[string]bool, error) {
	blockedOwners := make(map[string]bool)
	pending, err := delivery.outbox.pendingProjections(ctx)
	if err != nil {
		return 0, blockedOwners, err
	}
	replayed := 0
	for _, projection := range pending {
		rowKeys, err := delivery.outbox.projectionRowKeys(ctx, projection)
		if err == nil {
			err = delivery.applyProjection(ctx, projection, rowKeys)
		}
		if err != nil {
			blockedOwners[projection.OwnerID] = true
			continue
		}
		replayed++
	}
	return replayed, blockedOwners, nil
}

// reprojectEmbeddedOwners compares the metadata of every listed record that
// has committed occurrences with the metadata that the outbox recorded for
// the owner, and reprojects the owner when they differ or when a committed
// generation stored other metadata. Admission settings do not apply here. An
// owner that the index no longer lists keeps its last applied metadata.
func (w *conversationSemanticSyncWorker) reprojectEmbeddedOwners(
	ctx context.Context,
	store *embeddedConversationStore,
	stampedRecords []conversation.StampedRecord,
	blockedOwners map[string]bool,
	stats *embeddedSyncStats,
) {
	storedByOwner, err := store.outbox.ownerMetadata(ctx, store.namespace.ID)
	if err != nil {
		stats.reprojectionFailed++
		return
	}
	for _, stampedRecord := range stampedRecords {
		if semanticSyncContextDone(ctx) {
			return
		}
		ownerID := strings.TrimSpace(stampedRecord.Record.ID)
		stored, indexed := storedByOwner[ownerID]
		if !indexed || blockedOwners[ownerID] {
			continue
		}
		metadata := embeddedOwnerMetadataOf(stampedRecord.Record)
		if stored.Metadata == metadata && !stored.Stale {
			continue
		}
		if err := store.delivery.reproject(ctx, store.namespace.ID, ownerID, stored, metadata); err != nil {
			stats.reprojectionFailed++
			blockedOwners[ownerID] = true
			continue
		}
		storedByOwner[ownerID] = embeddedStoredOwnerMetadata{Metadata: metadata, ProjectionOrder: stored.ProjectionOrder + 1, Stale: false}
		stats.reprojectedOwners++
	}
}
