package vectorsearch

import (
	"context"
	"log/slog"
)

func newTimestampSourceRepairReport() TimestampSourceRepairReport {
	return TimestampSourceRepairReport{
		TimestampRepairReport: newTimestampRepairReport(),
		RowsNotZeroTime:       0,
		RowsEmptyContent:      0,
		RowsIncomplete:        0,
		Evidence:              nil,
		Outcomes:              map[string]int{},
	}
}

func logSourceTimestampRepairCompleted(
	ctx context.Context,
	collectionName string,
	milvus *milvusBackend,
	report *TimestampSourceRepairReport,
	dryRun bool,
) {
	slog.InfoContext(
		ctx,
		"conversation.vectorsearch.source_timestamp_repair_completed",
		"concern",
		"conversation.semantic",
		"component",
		"conversation",
		"collection",
		collectionName,
		"shared_layout",
		milvus.shared != nil,
		"conversations",
		report.Conversations,
		"rows_read",
		report.RowsRead,
		"rows_not_zero_time",
		report.RowsNotZeroTime,
		"rows_empty_content",
		report.RowsEmptyContent,
		"rows_incomplete",
		report.RowsIncomplete,
		"rows_current",
		report.RowsCurrent,
		"rows_changed",
		report.RowsChanged,
		"rows_written",
		report.RowsWritten,
		"rows_unmatched",
		report.RowsUnmatched,
		"rows_missing_timestamp",
		report.RowsMissingTimestamp,
		"rows_ambiguous",
		report.RowsAmbiguous,
		"rows_invalid_metadata",
		report.RowsInvalidMetadata,
		"dry_run",
		dryRun,
	)
}
