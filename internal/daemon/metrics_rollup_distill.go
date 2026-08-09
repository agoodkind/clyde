package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
)

// metricsRollupOverlap is how far behind the checkpoint a pass re-reads.
//
// A request occupies several log lines between its start and its terminal
// record. A pass that began exactly at the checkpoint would miss the start
// line of any request that finished just after it, and the aggregation would
// then see a terminal record with no retained start. Re-reading this much
// history covers requests longer than any adapter call is expected to run,
// and the checkpoint cursor drops whatever was already stored.
const metricsRollupOverlap = 30 * time.Minute

// metricsRollupDistillInput names one distill pass.
type metricsRollupDistillInput struct {
	LogPath      string
	RollupPath   string
	Now          time.Time
	LastRecordAt time.Time
	Pricing      adapterruntime.PricingTable
}

// metricsRollupDistillResult reports what one pass wrote.
type metricsRollupDistillResult struct {
	Written      int
	Pruned       int
	LastRecordAt time.Time
}

// distillMetricsRollup reads the recent daemon log and appends every finished
// request the store does not already hold.
//
// It reuses the log replay that the historical report already uses, so the
// rollup and a direct log read agree by construction rather than by two
// parsers staying in step.
//
// A cold start with no checkpoint scans the full retention window, which can
// be the largest single pass the distiller ever runs. ctx lets a reload's
// drain interrupt that scan between log rotations instead of waiting for it
// to finish or for the drain's own timeout to expire.
func distillMetricsRollup(ctx context.Context, input metricsRollupDistillInput) (metricsRollupDistillResult, error) {
	result := metricsRollupDistillResult{Written: 0, Pruned: 0, LastRecordAt: input.LastRecordAt}
	since := input.LastRecordAt.Add(-metricsRollupOverlap)
	if input.LastRecordAt.IsZero() {
		since = input.Now.Add(-metricsRollupRetention)
	}

	report := newRollupReport(MetricsWindow{Since: since, Until: input.Now})
	replay := MetricsHistoryInput{
		Since:   since,
		Now:     input.Now,
		LogPath: input.LogPath,
		Pricing: input.Pricing,
	}
	requests := make(map[string]*metricsRequest)
	for _, path := range daemonLogHistoryPaths(input.LogPath, since) {
		if err := ctx.Err(); err != nil {
			slog.WarnContext(ctx, "daemon.metrics_rollup.pass_canceled",
				"concern", "daemon.workers",
				"component", "daemon",
				"subcomponent", "metrics_rollup",
				"path", input.RollupPath,
				"err", err.Error(),
			)
			return result, fmt.Errorf("distill metrics rollup: %w", err)
		}
		readMetricsHistoryFile(path, replay, requests, &report)
	}

	records := make([]metricsRollupRecord, 0, len(requests))
	newest := input.LastRecordAt
	for requestID, request := range requests {
		if !distillableRequest(request, input.LastRecordAt, input.Now) {
			continue
		}
		records = append(records, requestToRollupRecord(requestID, request))
		if request.terminalAt.After(newest) {
			newest = request.terminalAt
		}
	}

	lock, err := acquireRollupWriteLock(input.RollupPath)
	if err != nil {
		return result, err
	}
	defer func() { _ = lock.Unlock() }()

	if len(records) > 0 {
		if err := appendMetricsRollupRecords(input.RollupPath, sortedRollupRecords(records)); err != nil {
			return result, err
		}
	}
	result.Written = len(records)
	result.LastRecordAt = newest

	pruned, err := pruneMetricsRollup(input.RollupPath, input.Now.Add(-metricsRollupRetention))
	if err != nil {
		return result, err
	}
	result.Pruned = pruned
	return result, nil
}

// distillableRequest reports whether a replayed request belongs in the store.
//
// Only finished requests are stored, because the aggregation counts a request
// in the window its terminal instant falls in. The checkpoint cursor is
// exclusive, which is what keeps the overlap re-read from duplicating records.
func distillableRequest(request *metricsRequest, lastRecordAt time.Time, now time.Time) bool {
	if request == nil || !request.terminal || request.invalidLifecycle {
		return false
	}
	if request.terminalAt.IsZero() || request.terminalAt.After(now) {
		return false
	}
	if !lastRecordAt.IsZero() && !request.terminalAt.After(lastRecordAt) {
		return false
	}
	return true
}
