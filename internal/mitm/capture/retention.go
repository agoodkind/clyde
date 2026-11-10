package capture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clock"
)

// RunRetention performs bounded maintenance until ctx or Close stops it.
// The caller owns the worker and must join it before closing the store.
func (s *Store) RunRetention(ctx context.Context) {
	if err := s.prepareRetention(ctx); err != nil {
		s.log.ErrorContext(ctx, "mitm.capture.retention_state_failed", "err", err)
		return
	}
	for {
		pending := s.prune(ctx)
		delay := s.cfg.RetentionInterval
		if pending {
			delay = 10 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.done:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Store) prepareRetention(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, retentionPrepareSQL, s.initialRequestID, s.openedAt.UnixNano()); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return fmt.Errorf("persist capture retention cutoff: %w", err)
	}
	return nil
}

func (s *Store) prune(ctx context.Context) bool {
	deadline := clock.Now().Add(100 * time.Millisecond)
	for {
		pending, err := s.maintainBatch(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.WarnContext(ctx, "mitm.capture.retention_failed", "err", err)
			}
			return false
		}
		if !pending || ctx.Err() != nil {
			return false
		}
		if clock.Now().After(deadline) {
			return true
		}
	}
}

func (s *Store) maintainBatch(ctx context.Context) (bool, error) {
	// Reclaim existing free pages before deleting retained history.
	before, err := s.diskBytes()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	pinned, err := s.reclaim(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	after, err := s.diskBytes()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	var cutoff, cutoffTime int64
	var complete int
	err = s.db.QueryRowContext(ctx, retentionProgressSQL).Scan(&cutoff, &cutoffTime, &complete)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("read retention progress: %w", err)
	}
	initial := err == nil && complete == 0
	if !initial && pinned && s.cfg.RetentionMaxBytes > 0 && after > s.cfg.RetentionMaxBytes {
		s.reportExcess(ctx, before, after, pinned, "checkpoint_pinned")
		return false, nil
	}
	var freePages int64
	if err := s.db.QueryRowContext(ctx, retentionFreelistSQL).Scan(&freePages); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	if !initial && freePages > 0 && s.cfg.RetentionMaxBytes > 0 && after > s.cfg.RetentionMaxBytes {
		s.reportExcess(ctx, before, after, pinned, "free_pages_pending")
		return after < before, nil
	}
	oldestBefore := int64(0)
	if s.cfg.RetentionMaxAge > 0 {
		oldestBefore = clock.Now().Add(-s.cfg.RetentionMaxAge).UnixNano()
	}
	sizeExceeded := s.cfg.RetentionMaxBytes > 0 && after > s.cfg.RetentionMaxBytes
	deleted, err := s.deleteBatch(ctx, initial, cutoff, cutoffTime, oldestBefore, sizeExceeded)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	if _, err := s.reclaim(ctx); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	after, err = s.diskBytes()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	if s.cfg.RetentionMaxBytes > 0 && after > s.cfg.RetentionMaxBytes {
		reason := "batch_pending"
		if deleted == 0 {
			reason = "no_progress"
		}
		s.reportExcess(ctx, before, after, pinned, reason)
	}
	return deleted > 0 || after < before, nil
}

func (s *Store) reportExcess(ctx context.Context, before, after int64, pinned bool, reason string) {
	now := clock.Now()
	if now.Sub(s.lastRetentionReport) < time.Second {
		return
	}
	s.lastRetentionReport = now
	s.log.WarnContext(ctx, "mitm.capture.retention_excess", "total_bytes", after, "max_bytes", s.cfg.RetentionMaxBytes, "excess_bytes", after-s.cfg.RetentionMaxBytes, "reclaimed_bytes", before-after, "pinned_readers", pinned, "reason", reason)
}

func (s *Store) diskBytes() (int64, error) {
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(s.cfg.DBPath + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			s.log.Warn("mitm.capture.retention_stat_failed", "path", s.cfg.DBPath+suffix, "err", err)
			return 0, fmt.Errorf("stat capture database%s: %w", suffix, err)
		}
		total += info.Size()
	}
	return total, nil
}

func (s *Store) reclaim(ctx context.Context) (pinned bool, resultError error) {
	connection, err := s.db.Conn(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("reserve capture reclamation connection: %w", err)
	}
	defer func() {
		resetContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if _, err := connection.ExecContext(resetContext, retentionRestoreBusyTimeoutSQL); err != nil {
			resultError = errors.Join(resultError, fmt.Errorf("restore capture writer timeout: %w", err))
		}
		if err := connection.Close(); err != nil {
			resultError = errors.Join(resultError, fmt.Errorf("release capture reclamation connection: %w", err))
		}
	}()
	if _, err := connection.ExecContext(ctx, retentionNonblockingSQL); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("set capture reclamation timeout: %w", err)
	}
	var busy, frames, checkpointed int
	if err := connection.QueryRowContext(ctx, retentionCheckpointPassiveSQL).Scan(&busy, &frames, &checkpointed); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("checkpoint capture database: %w", err)
	}
	if busy != 0 || checkpointed < frames {
		return true, nil
	}
	var mode int
	if err := connection.QueryRowContext(ctx, retentionVacuumModeSQL).Scan(&mode); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	if mode != 2 {
		if !s.reclamationDeferredReported {
			s.log.WarnContext(ctx, "mitm.capture.reclamation_deferred", "reason", "database_requires_offline_auto_vacuum_conversion")
			s.reclamationDeferredReported = true
		}
		return false, nil
	}
	rows, err := connection.QueryContext(ctx, retentionReclaimPagesSQL)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("reclaim capture pages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("reclaim capture pages: %w", err)
	}
	if err := rows.Close(); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("close capture reclamation: %w", err)
	}
	if err := connection.QueryRowContext(ctx, retentionCheckpointTruncateSQL).Scan(&busy, &frames, &checkpointed); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return false, fmt.Errorf("maintain capture retention: %w", err)
	}
	return busy != 0 || checkpointed < frames, nil
}

func (s *Store) deleteBatch(ctx context.Context, initial bool, cutoff, cutoffTime, oldestBefore int64, sizeExceeded bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return 0, fmt.Errorf("delete capture retention batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, retentionCreateSelectionSQL); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return 0, fmt.Errorf("delete capture retention batch: %w", err)
	}
	if _, err := tx.ExecContext(ctx, retentionClearSelectionSQL); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return 0, fmt.Errorf("delete capture retention batch: %w", err)
	}
	result, err := tx.ExecContext(ctx, retentionSelectRequestsSQL, initial, cutoff, initial, sizeExceeded, oldestBefore, oldestBefore, sizeExceeded, initial)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return 0, fmt.Errorf("delete capture retention batch: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return 0, fmt.Errorf("delete capture retention batch: %w", err)
	}
	queries := []string{
		retentionDeleteOrphanEventsSQL,
		retentionDeleteRequestEventsSQL,
		retentionDeleteDecodedBodiesSQL,
		retentionDeleteBodiesSQL,
		retentionDeleteRequestsSQL,
	}
	for _, query := range queries {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
			return 0, fmt.Errorf("delete capture retention batch: %w", err)
		}
	}
	for _, table := range []struct{ name, column string }{{"drift_shapes", "last_seen"}, {"drift_checks", "ts"}, {"baseline_diffs", "last_seen"}} {
		query := strings.NewReplacer("{{table}}", table.name, "{{column}}", table.column).Replace(retentionHistorySQL)
		result, err := tx.ExecContext(ctx, query, initial, cutoffTime, initial, sizeExceeded, oldestBefore, oldestBefore, sizeExceeded, initial)
		if err != nil {
			s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
			return 0, fmt.Errorf("delete capture retention batch: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
			return 0, fmt.Errorf("delete capture retention batch: %w", err)
		}
		deleted += count
	}
	if initial && deleted == 0 {
		if _, err := tx.ExecContext(ctx, retentionCompleteSQL); err != nil {
			s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
			return 0, fmt.Errorf("delete capture retention batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.retention_operation_failed", "err", err)
		return 0, fmt.Errorf("delete capture retention batch: %w", err)
	}
	return deleted, nil
}
