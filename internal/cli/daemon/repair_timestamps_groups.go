package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	// repairSourceGroupConversations limits the conversations in one write group.
	repairSourceGroupConversations = 50
	// repairSourceGroupRows closes a write group after this many changed rows.
	repairSourceGroupRows = 10_000
)

// sourceRepairGroup stores planned conversations until one shared write.
type sourceRepairGroup struct {
	plans         []vectorsearch.TimestampSourceRepairPlan
	records       []conversation.Record
	firstPosition int
	rowsChanged   int
}

func (group *sourceRepairGroup) add(position int, record conversation.Record, plan vectorsearch.TimestampSourceRepairPlan) {
	if len(group.records) == 0 {
		group.firstPosition = position
	}
	group.plans = append(group.plans, plan)
	group.records = append(group.records, record)
	group.rowsChanged += plan.Report.RowsChanged
}

func (group *sourceRepairGroup) full() bool {
	return len(group.records) >= repairSourceGroupConversations || group.rowsChanged >= repairSourceGroupRows
}

// repairSourceGroups plans records in bounded groups, writes each group once, and saves progress after each write.
func (run repairTimestampsRun) repairSourceGroups(ctx context.Context, progress timestampRepairProgress, retries []conversation.Record, pending []conversation.Record, state *repairLoopState) error {
	var group sourceRepairGroup
	for position, record := range append(slices.Clone(retries), pending...) {
		if state.report.RowsChanged+group.rowsChanged >= run.options.MaxRows || ctx.Err() != nil {
			if ctx.Err() == nil {
				if err := run.writeSourceGroup(ctx, progress, retries, &group, position, state); err != nil {
					return err
				}
			}
			state.failed = append(state.failed, retriesFrom(retries, group, position)...)
			return nil
		}
		plan, planErr := run.planSourceStoreRecord(ctx, record)
		if planErr != nil {
			return run.failSourceGroup(ctx, progress, retries, &group, position, record, plan, planErr, state)
		}
		group.add(position, record, plan)
		if group.full() {
			if err := run.writeSourceGroup(ctx, progress, retries, &group, position+1, state); err != nil {
				return err
			}
		}
	}
	return run.writeSourceGroup(ctx, progress, retries, &group, len(retries)+len(pending), state)
}

// retriesFrom returns unfinished retry IDs from the current group and later positions.
func retriesFrom(retries []conversation.Record, group sourceRepairGroup, position int) []string {
	first := position
	if len(group.records) > 0 {
		first = group.firstPosition
	}
	if first >= len(retries) {
		return nil
	}
	return recordIDs(retries[first:])
}

// planSourceStoreRecord loads one record's source and plans its timestamp updates.
func (run repairTimestampsRun) planSourceStoreRecord(ctx context.Context, record conversation.Record) (vectorsearch.TimestampSourceRepairPlan, error) {
	var empty vectorsearch.TimestampSourceRepairPlan
	source, loadErr := run.loadStoreSource(ctx, record)
	if loadErr != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.load_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", loadErr)
		readError := vectorsearch.SourceRepairEvidence{ConversationID: record.ID, Family: "", Rows: nil, Outcome: vectorsearch.SourceOutcomeReadError, Candidates: nil, Chosen: 0, Match: "", ToolCallID: "", ToolProjection: ""}
		return empty, errors.Join(fmt.Errorf("load source store: %w", loadErr), run.writeEvidence([]vectorsearch.SourceRepairEvidence{readError}))
	}
	plan, planErr := run.client.PlanTimestampsFromSource(ctx, run.collectionID, source)
	if planErr != nil {
		return plan, fmt.Errorf("repair timestamps from source store: %w", planErr)
	}
	return plan, nil
}

// failSourceGroup writes the earlier group, records the failed plan, saves progress, and returns the failure.
func (run repairTimestampsRun) failSourceGroup(ctx context.Context, progress timestampRepairProgress, retries []conversation.Record, group *sourceRepairGroup, position int, record conversation.Record, plan vectorsearch.TimestampSourceRepairPlan, planErr error, state *repairLoopState) error {
	if err := run.writeSourceGroup(ctx, progress, retries, group, position, state); err != nil {
		return errors.Join(fmt.Errorf("repair timestamps for %s: %w", record.ID, planErr), err)
	}
	run.sourceTotals.Add(plan.Report)
	state.report.Add(plan.Report.TimestampRepairReport)
	evidenceErr := run.writeEvidence(plan.Report.Evidence)
	state.failed = append(state.failed, retriesFrom(retries, *group, position)...)
	slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.record_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "cursor", state.cursor, "err", planErr)
	saveErr := run.saveProgress(progress, state.cursor, state.report, state.failed, false)
	return errors.Join(fmt.Errorf("repair timestamps for %s: %w", record.ID, planErr), evidenceErr, saveErr)
}

// writeSourceGroup writes one group, records its counts and evidence, advances the cursor, and saves progress.
func (run repairTimestampsRun) writeSourceGroup(ctx context.Context, progress timestampRepairProgress, retries []conversation.Record, group *sourceRepairGroup, nextPosition int, state *repairLoopState) error {
	if len(group.records) == 0 {
		return nil
	}
	written, writeErr := 0, error(nil)
	if !run.options.DryRun {
		written, writeErr = run.client.WriteTimestampSourcePlans(ctx, run.collectionID, group.plans)
	}
	evidenceErrs := make([]error, 0, len(group.plans))
	for _, plan := range group.plans {
		run.sourceTotals.Add(plan.Report)
		state.report.Add(plan.Report.TimestampRepairReport)
		evidenceErrs = append(evidenceErrs, run.writeEvidence(plan.Report.Evidence))
	}
	state.report.RowsWritten += written
	run.sourceTotals.RowsWritten += written
	evidenceErr := errors.Join(evidenceErrs...)
	first := group.records[0]
	if writeErr != nil {
		state.failed = append(state.failed, retriesFrom(retries, *group, nextPosition)...)
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.group_failed", "concern", "cli.daemon", "component", "cli", "first_conversation_id", first.ID, "conversations", len(group.records), "rows_written", written, "cursor", state.cursor, "err", writeErr)
		saveErr := run.saveProgress(progress, state.cursor, state.report, state.failed, false)
		return errors.Join(fmt.Errorf("repair timestamps for %s: %w", first.ID, writeErr), evidenceErr, saveErr)
	}
	for offset, record := range group.records {
		if group.firstPosition+offset >= len(retries) {
			state.cursor = record.ID
		}
	}
	*group = sourceRepairGroup{plans: nil, records: nil, firstPosition: 0, rowsChanged: 0}
	saveErr := run.saveProgress(progress, state.cursor, state.report, failedWithRemainingRetries(state.failed, retries, nextPosition), false)
	return errors.Join(evidenceErr, saveErr)
}
