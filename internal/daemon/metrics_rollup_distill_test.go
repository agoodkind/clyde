package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
)

func rollupPricingTable() adapterruntime.PricingTable {
	return adapterruntime.NewPricingTable(map[string]config.AdapterModelPricing{
		"claude-opus-4-8": {InputPerMTok: 5, OutputPerMTok: 25, CacheWritePerMTok: 0, CacheReadPerMTok: 0},
	})
}

// twoRequestLog is a daemon log holding two complete request lifecycles, one
// streaming and one not, so a replay of it exercises both stage shapes.
func twoRequestLog(t *testing.T) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-08-08T10:00:00Z", Message: "adapter.request.started", RequestID: "req-1"},
		{Time: "2026-08-08T10:00:00.030Z", Message: "adapter.request.stream_opened", RequestID: "req-1", Provider: "anthropic"},
		{
			Time: "2026-08-08T10:00:00.100Z", Message: "adapter.request.completed", RequestID: "req-1",
			DurationMS: 100, Provider: "anthropic", Model: "claude-opus-4-8",
			PromptTokens: 1000, CompletionTokens: 500,
		},
		{Time: "2026-08-08T10:00:00.100Z", Message: "adapter.request.io", RequestID: "req-1", BytesIn: 10, BytesOut: 20},
		{Time: "2026-08-08T10:05:00Z", Message: "adapter.request.started", RequestID: "req-2"},
		{
			Time: "2026-08-08T10:05:00.250Z", Message: "adapter.request.completed", RequestID: "req-2",
			DurationMS: 250, Provider: "anthropic", Model: "claude-opus-4-8",
			PromptTokens: 200, CompletionTokens: 40,
		},
		{Time: "2026-08-08T10:05:00.250Z", Message: "adapter.request.io", RequestID: "req-2", BytesIn: 5, BytesOut: 7},
	})
	return logPath
}

// TestRollupReportMatchesDirectLogReplay is the parity claim that justifies
// reading status from the rollup instead of the daemon log. Both paths run the
// same aggregation, so any field where they disagree is a distiller defect.
func TestRollupReportMatchesDirectLogReplay(t *testing.T) {
	t.Parallel()
	logPath := twoRequestLog(t)
	rollupPath := filepath.Join(t.TempDir(), metricsRollupFileName)
	now := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	pricing := rollupPricingTable()

	if _, err := distillMetricsRollup(t.Context(), metricsRollupDistillInput{
		LogPath: logPath, RollupPath: rollupPath, Now: now,
		LastRecordAt: time.Time{}, Pricing: pricing,
	}); err != nil {
		t.Fatalf("distill: %v", err)
	}

	direct := BuildMetricsHistory(MetricsHistoryInput{
		Since: now.Add(-2 * time.Hour), Now: now, LogPath: logPath, Pricing: pricing,
	})
	checkpointPath := filepath.Join(t.TempDir(), metricsRollupCheckpointFileName)
	fromRollup := metricsWindowsFromRollupPath(rollupPath, checkpointPath, []time.Duration{2 * time.Hour}, now, pricing)
	if len(fromRollup) != 1 {
		t.Fatalf("got %d windows, want 1", len(fromRollup))
	}
	rolled := fromRollup[0].Report

	// Guard the comparison: two empty reports would agree on every field, so
	// the parity assertions below only mean something once the direct replay
	// is known to have found both requests.
	if got := metricInt(direct.Metrics.Requests.Delta); got != 2 {
		t.Fatalf("direct replay found %d requests, want 2; parity below would be vacuous", got)
	}
	if metricInt(direct.TimeBreakdown.Total.TotalMS) == 0 {
		t.Fatal("direct replay measured no duration; stage parity below would be vacuous")
	}

	assertCounterEqual(t, "requests", direct.Metrics.Requests, rolled.Metrics.Requests)
	assertCounterEqual(t, "completed", direct.Metrics.Completed, rolled.Metrics.Completed)
	assertCounterEqual(t, "bytes_in", direct.Metrics.BytesIn, rolled.Metrics.BytesIn)
	assertCounterEqual(t, "bytes_out", direct.Metrics.BytesOut, rolled.Metrics.BytesOut)
	assertCounterEqual(t, "input_tokens", direct.Metrics.InputTokens, rolled.Metrics.InputTokens)
	assertCounterEqual(t, "output_tokens", direct.Metrics.OutputTokens, rolled.Metrics.OutputTokens)
	assertCounterEqual(t, "cost_microcents", direct.Metrics.CostMicrocents, rolled.Metrics.CostMicrocents)

	if metricInt(direct.TimeBreakdown.Total.TotalMS) != metricInt(rolled.TimeBreakdown.Total.TotalMS) {
		t.Fatalf("total duration: direct=%v rollup=%v",
			metricInt(direct.TimeBreakdown.Total.TotalMS), metricInt(rolled.TimeBreakdown.Total.TotalMS))
	}
	for _, stage := range []string{"provider_to_first_response", "response_streaming", "provider_total"} {
		directStage, directOK := findStage(direct, stage)
		rolledStage, rolledOK := findStage(rolled, stage)
		if directOK != rolledOK {
			t.Fatalf("stage %s present direct=%v rollup=%v", stage, directOK, rolledOK)
		}
		if directOK && metricInt(directStage.TotalMS) != metricInt(rolledStage.TotalMS) {
			t.Fatalf("stage %s total: direct=%v rollup=%v",
				stage, metricInt(directStage.TotalMS), metricInt(rolledStage.TotalMS))
		}
	}
}

func assertCounterEqual(t *testing.T, name string, direct MetricsCounter, rolled MetricsCounter) {
	t.Helper()
	if metricInt(direct.Delta) != metricInt(rolled.Delta) {
		t.Fatalf("%s delta: direct=%v rollup=%v", name, metricInt(direct.Delta), metricInt(rolled.Delta))
	}
}

func findStage(report MetricsHistoryReport, name string) (MetricsDuration, bool) {
	for _, stage := range report.TimeBreakdown.Stages {
		if stage.Name == name {
			return stage.MetricsDuration, true
		}
	}
	return MetricsDuration{}, false
}

// TestDistillIsIdempotentAcrossPasses proves the checkpoint cursor works: a
// second pass over an unchanged log adds nothing, so counts cannot inflate
// each time the background worker runs.
func TestDistillIsIdempotentAcrossPasses(t *testing.T) {
	t.Parallel()
	logPath := twoRequestLog(t)
	rollupPath := filepath.Join(t.TempDir(), metricsRollupFileName)
	now := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)
	pricing := rollupPricingTable()

	first, err := distillMetricsRollup(t.Context(), metricsRollupDistillInput{
		LogPath: logPath, RollupPath: rollupPath, Now: now,
		LastRecordAt: time.Time{}, Pricing: pricing,
	})
	if err != nil {
		t.Fatalf("first distill: %v", err)
	}
	if first.Written != 2 {
		t.Fatalf("first pass wrote %d records, want 2", first.Written)
	}

	second, err := distillMetricsRollup(t.Context(), metricsRollupDistillInput{
		LogPath: logPath, RollupPath: rollupPath, Now: now,
		LastRecordAt: first.LastRecordAt, Pricing: pricing,
	})
	if err != nil {
		t.Fatalf("second distill: %v", err)
	}
	if second.Written != 0 {
		t.Fatalf("second pass wrote %d records over an unchanged log, want 0", second.Written)
	}

	checkpointPath := filepath.Join(t.TempDir(), metricsRollupCheckpointFileName)
	reports := metricsWindowsFromRollupPath(rollupPath, checkpointPath, []time.Duration{2 * time.Hour}, now, pricing)
	if got := metricInt(reports[0].Report.Metrics.Requests.Delta); got != 2 {
		t.Fatalf("requests after two passes = %d, want 2", got)
	}
}

// TestDistillSkipsRequestsWithNoTerminalRecord keeps an in-flight request out
// of the store. Storing it would count a request in a window before it
// finished, and the record could never be corrected because the store is
// append-only.
func TestDistillSkipsRequestsWithNoTerminalRecord(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "clyde-daemon.jsonl")
	writeMetricsHistoryRecords(t, logPath, []metricsHistoryRecord{
		{Time: "2026-08-08T10:00:00Z", Message: "adapter.request.started", RequestID: "req-open"},
		{Time: "2026-08-08T10:00:00.030Z", Message: "adapter.request.stream_opened", RequestID: "req-open"},
	})
	rollupPath := filepath.Join(t.TempDir(), metricsRollupFileName)
	now := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)

	result, err := distillMetricsRollup(t.Context(), metricsRollupDistillInput{
		LogPath: logPath, RollupPath: rollupPath, Now: now,
		LastRecordAt: time.Time{}, Pricing: rollupPricingTable(),
	})
	if err != nil {
		t.Fatalf("distill: %v", err)
	}
	if result.Written != 0 {
		t.Fatalf("wrote %d records for an unfinished request, want 0", result.Written)
	}
}

// TestDefaultWindowsReportIndependentTotals is the behavior the user sees: one
// read yields three windows, and a request that finished two days ago counts
// in the week but not in the hour or the day.
func TestDefaultWindowsReportIndependentTotals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	rollupPath := writeRollupFixture(t, []metricsRollupRecord{
		requestRollupFixture("in-hour", "2026-08-08T11:30:00Z", "", "2026-08-08T11:30:02Z"),
		requestRollupFixture("in-day", "2026-08-08T02:00:00Z", "", "2026-08-08T02:00:02Z"),
		requestRollupFixture("in-week", "2026-08-06T02:00:00Z", "", "2026-08-06T02:00:02Z"),
	})

	checkpointPath := writeRollupCheckpointFixture(t, now)
	reports := metricsWindowsFromRollupPath(rollupPath, checkpointPath, defaultMetricsWindows, now, rollupPricingTable())
	if len(reports) != 3 {
		t.Fatalf("got %d windows, want 3", len(reports))
	}
	wantLabels := []string{"1h", "1d", "1w"}
	wantRequests := []int64{1, 2, 3}
	for i, report := range reports {
		if report.Label != wantLabels[i] {
			t.Fatalf("window %d label = %q, want %q", i, report.Label, wantLabels[i])
		}
		if got := metricInt(report.Report.Metrics.Requests.Delta); got != wantRequests[i] {
			t.Fatalf("window %s requests = %d, want %d", report.Label, got, wantRequests[i])
		}
	}
}

// TestWindowReportsRestartCount checks the reset decision: totals stay summed
// across a restart and the window says how many restarts it absorbed.
func TestWindowReportsRestartCount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	rollupPath := writeRollupFixture(t, []metricsRollupRecord{
		requestRollupFixture("before", "2026-08-08T11:00:00Z", "", "2026-08-08T11:00:02Z"),
		generationRollupRecord(time.Date(2026, 8, 8, 11, 30, 0, 0, time.UTC)),
		requestRollupFixture("after", "2026-08-08T11:45:00Z", "", "2026-08-08T11:45:02Z"),
	})

	checkpointPath := writeRollupCheckpointFixture(t, now)
	reports := metricsWindowsFromRollupPath(rollupPath, checkpointPath, []time.Duration{2 * time.Hour}, now, rollupPricingTable())
	report := reports[0].Report
	if report.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", report.Restarts)
	}
	if got := metricInt(report.Metrics.Requests.Delta); got != 2 {
		t.Fatalf("requests = %d, want 2 summed across the restart", got)
	}
	if !hasWarningContaining(report, "restart") {
		t.Fatalf("warnings = %v, want one naming the restart", report.Warnings)
	}
}

func hasWarningContaining(report MetricsHistoryReport, needle string) bool {
	for _, warning := range report.Warnings {
		if strings.Contains(warning, needle) {
			return true
		}
	}
	return false
}

// writeRollupCheckpointFixture writes a checkpoint whose pass just completed
// at now, so a window ending at now reads as caught up to the trailing edge.
// Most fixture-driven tests want this: they are asserting something about the
// store's content, not about distill staleness, and a missing checkpoint would
// otherwise mark every one of them Coverage.Complete=false for a reason
// unrelated to what they are testing.
func writeRollupCheckpointFixture(t *testing.T, passCompletedAt time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), metricsRollupCheckpointFileName)
	if err := writeMetricsRollupCheckpoint(path, metricsRollupCheckpoint{
		LastRecordAt: "",
		LastPassAt:   formatRollupTime(passCompletedAt),
	}); err != nil {
		t.Fatalf("write checkpoint fixture: %v", err)
	}
	return path
}

// TestEmptyRollupReportsNoHistoryRatherThanZero separates "nothing happened"
// from "nothing recorded". A fresh install must not read as a quiet week.
func TestEmptyRollupReportsNoHistoryRatherThanZero(t *testing.T) {
	t.Parallel()
	rollupPath := filepath.Join(t.TempDir(), metricsRollupFileName)
	checkpointPath := filepath.Join(t.TempDir(), metricsRollupCheckpointFileName)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	reports := metricsWindowsFromRollupPath(rollupPath, checkpointPath, defaultMetricsWindows, now, rollupPricingTable())
	for _, report := range reports {
		if report.Report.Coverage.Complete {
			t.Fatalf("window %s claims complete coverage with no store", report.Label)
		}
		if report.Report.Metrics.Requests.Delta != nil {
			t.Fatalf("window %s reported a request delta with no store", report.Label)
		}
		if !hasWarningContaining(report.Report, "no distilled daemon history") {
			t.Fatalf("window %s warnings = %v, want the missing-history warning", report.Label, report.Report.Warnings)
		}
	}
}

// TestWindowClearsCoverageWhenDistillerLagsBehindWindowEnd is the coverage-lag
// finding: the distiller runs on an interval, so a read taken just before the
// next pass can be missing up to that long of the most recent traffic. Without
// checking the checkpoint's pass time, Coverage.Complete would say the window
// is trustworthy right up to Window.Until when it is not.
func TestWindowClearsCoverageWhenDistillerLagsBehindWindowEnd(t *testing.T) {
	t.Parallel()
	now := rollupTestTime(t, "2026-08-08T12:00:00Z")
	rollupPath := writeRollupFixture(t, []metricsRollupRecord{
		requestRollupFixture("in-window", "2026-08-08T11:30:00Z", "", "2026-08-08T11:30:02Z"),
	})
	// The last pass finished 12 minutes before now, well past
	// metricsRollupInterval (5 minutes), so the store cannot be trusted to hold
	// everything up to Window.Until.
	staleCheckpoint := writeRollupCheckpointFixture(t, now.Add(-12*time.Minute))

	reports := metricsWindowsFromRollupPath(rollupPath, staleCheckpoint, []time.Duration{time.Hour}, now, rollupPricingTable())
	report := reports[0].Report
	if report.Coverage.Complete {
		t.Fatal("coverage claims complete with a distill pass 12 minutes stale")
	}
	if !hasWarningContaining(report, "distilled history may be up to") {
		t.Fatalf("warnings = %v, want one naming the distill lag", report.Warnings)
	}
}

// TestWindowReportsCompleteCoverageWhenDistillerIsCurrent is the positive case:
// a fresh checkpoint and history reaching back to the window start together
// mean the reader can trust the numbers.
func TestWindowReportsCompleteCoverageWhenDistillerIsCurrent(t *testing.T) {
	t.Parallel()
	now := rollupTestTime(t, "2026-08-08T12:00:00Z")
	rollupPath := writeRollupFixture(t, []metricsRollupRecord{
		generationRollupRecord(now.Add(-90 * time.Minute)),
		requestRollupFixture("in-window", "2026-08-08T11:30:00Z", "", "2026-08-08T11:30:02Z"),
	})
	freshCheckpoint := writeRollupCheckpointFixture(t, now.Add(-1*time.Minute))

	reports := metricsWindowsFromRollupPath(rollupPath, freshCheckpoint, []time.Duration{time.Hour}, now, rollupPricingTable())
	report := reports[0].Report
	if !report.Coverage.Complete {
		t.Fatalf("coverage = incomplete warnings=%v, want complete with a current pass and full history", report.Warnings)
	}
}

// TestDistillMetricsRollupStopsPromptlyWhenCanceled is the cancellation
// finding: a cold-start pass with no checkpoint scans the full retention
// window, and a reload's drain must be able to interrupt that scan rather than
// wait for it to finish.
func TestDistillMetricsRollupStopsPromptlyWhenCanceled(t *testing.T) {
	t.Parallel()
	logPath := twoRequestLog(t)
	rollupPath := filepath.Join(t.TempDir(), metricsRollupFileName)
	now := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := distillMetricsRollup(ctx, metricsRollupDistillInput{
		LogPath: logPath, RollupPath: rollupPath, Now: now,
		LastRecordAt: time.Time{}, Pricing: rollupPricingTable(),
	})
	if err == nil {
		t.Fatal("distill with a canceled context returned no error")
	}
	if _, statErr := os.Stat(rollupPath); !os.IsNotExist(statErr) {
		t.Fatalf("distill wrote to the store despite cancellation before any log read: stat err=%v", statErr)
	}
}

// TestDistillMetricsRollupWaitsForTheWriteLock is the reload-overlap finding:
// two processes distilling the same store concurrently must not interleave
// their append-and-prune passes, since a prune's scan-then-rename can lose
// lines the other process appended in between. This test proves the lock is
// real by holding it externally and observing the distill call block until it
// is released.
func TestDistillMetricsRollupWaitsForTheWriteLock(t *testing.T) {
	t.Parallel()
	logPath := twoRequestLog(t)
	rollupPath := filepath.Join(t.TempDir(), metricsRollupFileName)
	now := time.Date(2026, 8, 8, 11, 0, 0, 0, time.UTC)

	holder := flock.New(rollupPath + ".lock")
	if err := holder.Lock(); err != nil {
		t.Fatalf("acquire test lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := distillMetricsRollup(t.Context(), metricsRollupDistillInput{
			LogPath: logPath, RollupPath: rollupPath, Now: now,
			LastRecordAt: time.Time{}, Pricing: rollupPricingTable(),
		})
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("distill returned (err=%v) while the write lock was still held", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := holder.Unlock(); err != nil {
		t.Fatalf("release test lock: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("distill after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("distill did not proceed after the write lock was released")
	}
}
