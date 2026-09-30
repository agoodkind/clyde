package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/clock"
)

// Library write batch defaults for zero max_batch_rows and max_batch_bytes.
// They equal the library's own defaults.
const (
	embeddedDefaultMaxBatchRows  = 256
	embeddedDefaultMaxBatchBytes = 8 << 20
)

// embeddedConversationDelivery writes owner generations through the outbox and
// the library. It records every generation in the outbox before the first
// library call, stages the rows in bounded batches, commits the generation
// with its seal, and acknowledges the receipt in the outbox.
type embeddedConversationDelivery struct {
	library       *library.Library
	outbox        *conversationSemanticOutbox
	maxBatchRows  int
	maxBatchBytes int64
	// maxReplayBytes bounds the stored row bytes that one replay attempts.
	maxReplayBytes int64
	log            *slog.Logger
}

// embeddedGeneration is one owner generation: its outbox batch, its rows, and
// the seal of all rows.
type embeddedGeneration struct {
	batch embeddedOutboxBatch
	rows  []embeddedOutboxRow
	seal  library.GenerationSeal
}

// embeddedDeliveryCounts counts the library work of one delivery or replay.
type embeddedDeliveryCounts struct {
	// recordedBatches counts generations written to the outbox.
	recordedBatches int
	// stagedRows counts rows sent to Stage, which embeds and writes the
	// vectors that the catalog lacks.
	stagedRows int
	// stageMilliseconds totals the time spent in Stage.
	stageMilliseconds int64
	// committedGenerations and committedRows count receipts that the outbox
	// acknowledged.
	committedGenerations int
	committedRows        int
}

func (counts *embeddedDeliveryCounts) add(other embeddedDeliveryCounts) {
	counts.recordedBatches += other.recordedBatches
	counts.stagedRows += other.stagedRows
	counts.stageMilliseconds += other.stageMilliseconds
	counts.committedGenerations += other.committedGenerations
	counts.committedRows += other.committedRows
}

// prepareGeneration builds the next generation of one owner. The generation
// order is the committed order plus one. The idempotency token is the SHA-256
// of the length-prefixed namespace, owner ID, order, and manifest hash.
func (delivery *embeddedConversationDelivery) prepareGeneration(
	ctx context.Context,
	batch embeddedOutboxBatch,
	rows []embeddedOutboxRow,
) (embeddedGeneration, error) {
	occurrences := make([]library.Occurrence, 0, len(rows))
	for _, row := range rows {
		occurrences = append(occurrences, row.Occurrence)
	}
	seal, err := library.SealRows(occurrences)
	if err != nil {
		delivery.log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.seal_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", batch.OwnerID,
			"err", err,
		)
		return embeddedGeneration{}, fmt.Errorf("seal generation of %s: %w", batch.OwnerID, err)
	}
	state, err := delivery.library.GetOwnerState(ctx, batch.Namespace, batch.OwnerID)
	if err != nil {
		delivery.log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.owner_state_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", batch.OwnerID,
			"err", err,
		)
		return embeddedGeneration{}, fmt.Errorf("read owner state of %s: %w", batch.OwnerID, err)
	}
	batch.GenerationOrder = state.GenerationOrder + 1
	batch.RowCount = seal.RowCount
	batch.ManifestHash = seal.ManifestHash
	batch.BatchID = embeddedGenerationToken(batch.Namespace, batch.OwnerID, batch.GenerationOrder, seal.ManifestHash)
	return embeddedGeneration{batch: batch, rows: rows, seal: seal}, nil
}

// embeddedGenerationToken returns the lowercase hex SHA-256 of the
// length-prefixed namespace, owner ID, decimal generation order, and manifest
// hash.
func embeddedGenerationToken(namespace string, ownerID string, generationOrder uint64, manifestHash string) string {
	hasher := sha256.New()
	for _, value := range []string{namespace, ownerID, strconv.FormatUint(generationOrder, 10), manifestHash} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hasher.Write(length[:])
		hasher.Write([]byte(value))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// deliver records one generation in the outbox and then stages, commits, and
// acknowledges it.
func (delivery *embeddedConversationDelivery) deliver(ctx context.Context, generation embeddedGeneration) (embeddedDeliveryCounts, error) {
	var counts embeddedDeliveryCounts
	if err := delivery.outbox.recordBatch(ctx, generation.batch, generation.rows); err != nil {
		return counts, err
	}
	counts.recordedBatches++
	published, err := delivery.publish(ctx, generation)
	counts.add(published)
	return counts, err
}

// publish stages, commits, and acknowledges one generation after the outbox
// recorded it. A permanent library error moves the batch to the blocked state.
func (delivery *embeddedConversationDelivery) publish(ctx context.Context, generation embeddedGeneration) (embeddedDeliveryCounts, error) {
	ctx = embeddedGenerationObservationContext(ctx, generation.batch)
	var counts embeddedDeliveryCounts
	staged, err := delivery.stage(ctx, generation)
	counts.add(staged)
	if err != nil {
		delivery.blockBatchOnPermanentError(ctx, generation.batch, err)
		return counts, err
	}
	receipt, err := delivery.commit(ctx, generation)
	if err != nil {
		delivery.blockBatchOnPermanentError(ctx, generation.batch, err)
		return counts, err
	}
	if err := delivery.outbox.acknowledgeBatch(ctx, generation.batch, receipt.Fingerprint); err != nil {
		return counts, err
	}
	counts.committedGenerations++
	counts.committedRows += len(generation.rows)
	return counts, nil
}

// stage sends the rows of one generation to Stage in batches bounded by the
// configured row and byte limits. The byte count of a row is the byte length
// of its source text, search text, and embedding input, which the library
// compares with MaxBatchBytes.
func (delivery *embeddedConversationDelivery) stage(ctx context.Context, generation embeddedGeneration) (embeddedDeliveryCounts, error) {
	var counts embeddedDeliveryCounts
	key := embeddedGenerationKey(generation.batch)
	started := clock.Now()
	chunk := make([]library.Occurrence, 0, delivery.maxBatchRows)
	var chunkBytes int64
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := delivery.library.Stage(ctx, library.StageBatch{Key: key, Mode: library.Append, Rows: chunk}); err != nil {
			return fmt.Errorf("stage %d rows: %w", len(chunk), err)
		}
		counts.stagedRows += len(chunk)
		chunk = make([]library.Occurrence, 0, delivery.maxBatchRows)
		chunkBytes = 0
		return nil
	}
	var stageErr error
	for _, row := range generation.rows {
		rowBytes := int64(len(row.Occurrence.SourceText) + len(row.Occurrence.SearchText) + len(row.Occurrence.EmbeddingInput))
		if len(chunk) == delivery.maxBatchRows || (len(chunk) > 0 && chunkBytes+rowBytes > delivery.maxBatchBytes) {
			if stageErr = flush(); stageErr != nil {
				break
			}
		}
		chunk = append(chunk, row.Occurrence)
		chunkBytes += rowBytes
	}
	if stageErr == nil {
		stageErr = flush()
	}
	counts.stageMilliseconds = clock.Since(started).Milliseconds()
	if stageErr != nil {
		delivery.log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.stage_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", generation.batch.OwnerID,
			"generation_order", generation.batch.GenerationOrder,
			"staged_rows", counts.stagedRows,
			"err", stageErr,
		)
		return counts, fmt.Errorf("stage generation %d of %s: %w", generation.batch.GenerationOrder, generation.batch.OwnerID, stageErr)
	}
	return counts, nil
}

// commit publishes a staged generation with its seal and requires a receipt
// for the generation order of the batch. CommitGeneration returns the saved
// receipt for a token that the library already committed.
func (delivery *embeddedConversationDelivery) commit(ctx context.Context, generation embeddedGeneration) (library.ApplyReceipt, error) {
	receipt, err := delivery.library.CommitGeneration(ctx, embeddedGenerationKey(generation.batch), generation.seal)
	if err == nil && receipt.GenerationOrder != generation.batch.GenerationOrder {
		err = fmt.Errorf("receipt order %d differs from batch order %d", receipt.GenerationOrder, generation.batch.GenerationOrder)
	}
	if err != nil {
		delivery.log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.commit_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", generation.batch.OwnerID,
			"generation_order", generation.batch.GenerationOrder,
			"err", err,
		)
		return library.ApplyReceipt{}, fmt.Errorf("commit generation %d of %s: %w", generation.batch.GenerationOrder, generation.batch.OwnerID, err)
	}
	return receipt, nil
}

func embeddedGenerationKey(batch embeddedOutboxBatch) library.GenerationKey {
	return library.GenerationKey{
		Namespace:        batch.Namespace,
		OwnerID:          batch.OwnerID,
		GenerationOrder:  batch.GenerationOrder,
		IdempotencyToken: batch.BatchID,
	}
}

// embeddedReplayResult reports one replay of the pending outbox batches.
type embeddedReplayResult struct {
	counts embeddedDeliveryCounts
	// replayed counts pending batches that this replay acknowledged.
	replayed int
	// deferred counts pending batches that this replay did not attempt after
	// the replay stopped.
	deferred int
	// blockedOwners lists owners with a pending batch that this replay did not
	// acknowledge, and owners with a blocked batch or projection. A new
	// generation for such an owner would take the same order. A pass delivers
	// no new work and applies no reprojection for these owners.
	blockedOwners map[string]bool
}

// errReplayStoredRows wraps a replay failure that reading or sealing the stored
// rows of one batch caused, before any library call.
var errReplayStoredRows = errors.New("stored outbox rows are unreadable or do not match their seal")

// replayPending publishes pending outbox batches from their stored rows in the
// order the outbox recorded them. The stored seal must equal the seal of the
// stored rows. CommitGeneration returns the saved receipt for a token that the
// library already committed, and the replay acknowledges that receipt. A
// blocked batch is not pending and is never replayed.
//
// One replay is bounded. It stops at the first batch that fails with a
// transient library or embedding error. The next batch would call the same
// endpoint while the pass keeps the store locked. The replay also starts a
// batch only when no batch started yet or when the stored row bytes of the
// attempted batches plus this batch stay within maxReplayBytes, the 8 MiB
// delivery byte target. A permanent library error or a stored-row error does
// not stop the replay. Every batch that the replay did not acknowledge,
// attempted or not, keeps its owner out of delivery for this pass.
func (delivery *embeddedConversationDelivery) replayPending(ctx context.Context) (embeddedReplayResult, error) {
	var result embeddedReplayResult
	result.blockedOwners = make(map[string]bool)
	pending, err := delivery.outbox.pendingBatches(ctx)
	if err != nil {
		return result, err
	}
	var attemptedBytes int64
	stopped := false
	for _, batch := range pending {
		if stopped || ctx.Err() != nil {
			result.blockedOwners[batch.OwnerID] = true
			result.deferred++
			continue
		}
		rows, rowsErr := delivery.outbox.batchRows(ctx, batch.BatchID)
		rowBytes := embeddedOutboxRowBytes(rows)
		if attemptedBytes > 0 && attemptedBytes+rowBytes > delivery.maxReplayBytes {
			stopped = true
			result.blockedOwners[batch.OwnerID] = true
			result.deferred++
			continue
		}
		attemptedBytes += rowBytes
		counts, err := delivery.replayBatch(ctx, batch, rows, rowsErr)
		result.counts.add(counts)
		if err == nil {
			result.replayed++
			continue
		}
		result.blockedOwners[batch.OwnerID] = true
		if _, permanent := permanentLibraryErrorClass(err); !permanent && !errors.Is(err, errReplayStoredRows) {
			stopped = true
		}
	}
	if result.deferred > 0 {
		delivery.log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.replay_deferred",
			"concern", "conversation.semantic",
			"component", "daemon",
			"deferred_batches", result.deferred,
			"attempted_bytes", attemptedBytes,
			"max_replay_bytes", delivery.maxReplayBytes,
		)
	}
	return result, nil
}

// embeddedOutboxRowBytes returns the byte length of the source text, search
// text, and embedding input of rows.
func embeddedOutboxRowBytes(rows []embeddedOutboxRow) int64 {
	var total int64
	for _, row := range rows {
		total += int64(len(row.Occurrence.SourceText) + len(row.Occurrence.SearchText) + len(row.Occurrence.EmbeddingInput))
	}
	return total
}

func (delivery *embeddedConversationDelivery) replayBatch(
	ctx context.Context,
	batch embeddedOutboxBatch,
	rows []embeddedOutboxRow,
	rowsErr error,
) (embeddedDeliveryCounts, error) {
	occurrences := make([]library.Occurrence, 0, len(rows))
	for _, row := range rows {
		occurrences = append(occurrences, row.Occurrence)
	}
	err := rowsErr
	var seal library.GenerationSeal
	if err == nil {
		seal, err = library.SealRows(occurrences)
	}
	if err == nil && (seal.RowCount != batch.RowCount || seal.ManifestHash != batch.ManifestHash) {
		err = fmt.Errorf("stored rows seal %d rows with manifest %s, the batch states %d rows with manifest %s",
			seal.RowCount, seal.ManifestHash, batch.RowCount, batch.ManifestHash)
	}
	if err != nil {
		delivery.log.WarnContext(
			ctx, "daemon.conversation_semantic_embedded.replay_seal_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", batch.OwnerID,
			"batch_id", batch.BatchID,
			"err", err,
		)
		return embeddedDeliveryCounts{}, fmt.Errorf("replay batch %s of %s: %w: %w", batch.BatchID, batch.OwnerID, errReplayStoredRows, err)
	}
	return delivery.publish(ctx, embeddedGeneration{batch: batch, rows: rows, seal: seal})
}
