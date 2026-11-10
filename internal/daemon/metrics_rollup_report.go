package daemon

import (
	"fmt"
	"os"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

// defaultMetricsWindows are the windows the status command reports without
// being asked. They answer three different questions from one read: what is
// happening now, what today looked like, and what the week looked like.
var defaultMetricsWindows = []time.Duration{
	time.Hour,
	24 * time.Hour,
	7 * 24 * time.Hour,
}

// MetricsWindowReport pairs one reported window with the label the surfaces
// print for it.
type MetricsWindowReport struct {
	Label  string               `json:"label"`
	Report MetricsHistoryReport `json:"report"`
}

// metricsWindowLabel renders a window duration the way the surfaces print it.
func metricsWindowLabel(window time.Duration) string {
	switch {
	case window%(24*time.Hour) == 0 && window >= 7*24*time.Hour:
		return fmt.Sprintf("%dw", int(window/(7*24*time.Hour)))
	case window%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(window/(24*time.Hour)))
	default:
		return fmt.Sprintf("%dh", int(window/time.Hour))
	}
}

// MetricsWindowsFromRollup reports every default window from one read of the
// distilled store.
//
// The store is small and pre-distilled, so this stays fast enough to run on
// every status invocation, which is what lets the command report history
// without being asked for a window.
func MetricsWindowsFromRollup(now time.Time) []MetricsWindowReport {
	return metricsWindowsFromRollupPath(
		metricsRollupPath(), metricsRollupCheckpointPath(), defaultMetricsWindows, now, loadRollupPricing(),
		currentRollupSourcePosition())
}

func currentRollupSourcePosition() metricsRollupSourcePosition {
	var unknown metricsRollupSourcePosition
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return unknown
	}
	logPath := slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon)
	info, err := os.Stat(logPath)
	if err != nil {
		return unknown
	}
	return rollupSourcePosition(logPath, info)
}

// loadRollupPricing resolves the pricing table the cost counter needs. A
// config that will not load yields an empty table, which prices nothing and
// leaves the cost counter absent rather than reporting a wrong number.
func loadRollupPricing() adapterruntime.PricingTable {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return adapterruntime.NewPricingTable(nil)
	}
	return adapterruntime.NewPricingTable(cfg.Adapter.ModelPricing())
}

// metricsWindowsFromRollupPath is the testable core of
// [MetricsWindowsFromRollup]. checkpointPath is separate from path because
// production resolves both from the state dir, but a test isolates the
// rollup file in its own temp dir and must isolate the checkpoint the same
// way, or a coverage-lag check here would read the real host's checkpoint
// instead of the fixture's.
func metricsWindowsFromRollupPath(
	path string,
	checkpointPath string,
	durations []time.Duration,
	now time.Time,
	pricing adapterruntime.PricingTable,
	currentSource metricsRollupSourcePosition,
) []MetricsWindowReport {
	windows := make([]MetricsWindow, 0, len(durations))
	for _, duration := range durations {
		windows = append(windows, MetricsWindow{Since: now.Add(-duration), Until: now})
	}

	checkpoint := readMetricsRollupCheckpoint(checkpointPath)
	lastPassAt, _ := parseRollupTime(checkpoint.LastPassAt)
	if !lastPassAt.IsZero() && rollupSourceCaughtUp(checkpoint.Source, currentSource) {
		lastPassAt = now
	}
	coverageSince, hasCoverage := parseRollupTime(checkpoint.CoverageSince)

	reports := make([]MetricsWindowReport, 0, len(durations))
	loaded, loadErr := loadMetricsRollup(path, windows)
	for i, duration := range durations {
		report := newRollupReport(windows[i])
		if loadErr != nil {
			addMetricsWarning(&report, "unreadable metrics rollup store")
			report.invalidHistory = true
			reports = append(reports, MetricsWindowReport{Label: metricsWindowLabel(duration), Report: report})
			continue
		}
		applyRollupWindow(&report, loaded[i], lastPassAt, pricing)
		if !hasCoverage || coverageSince.After(windows[i].Since) {
			report.Coverage.Complete = false
			addMetricsWarning(&report, "historical summary coverage is unavailable before the current log continuation")
		}
		reports = append(reports, MetricsWindowReport{Label: metricsWindowLabel(duration), Report: report})
	}
	return reports
}

func rollupSourceCaughtUp(saved metricsRollupSourcePosition, current metricsRollupSourcePosition) bool {
	return saved.Path != "" && saved == current
}

// newRollupReport builds the empty report one window starts from.
func newRollupReport(window MetricsWindow) MetricsHistoryReport {
	return MetricsHistoryReport{
		Window:                 window,
		Coverage:               MetricsCoverage{Complete: false},
		Metrics:                emptyMetricsValues(),
		TimeBreakdown:          MetricsTimeBreakdown{Total: emptyMetricsDuration(), Stages: []MetricsStageDuration{}},
		UnattributedDurationMS: nil,
		Restarts:               0,
		Warnings:               []string{},
		historyCoversStart:     false,
		liveCoversEnd:          false,
		generationStable:       true,
		invalidHistory:         false,
		sawCanonicalRecord:     false,
	}
}

// applyRollupWindow folds one loaded window into its report.
//
// A restart inside the window is reported rather than used to invalidate the
// counters: the store holds per-request records, so summing across a restart
// is exact, and the restart count is what tells the reader the window spans a
// discontinuity.
//
// The report uses the report time when saved source path, device, inode,
// and offset match the current log file.
// Stored data can lag the report window end by up to metricsRollupInterval.
func applyRollupWindow(
	report *MetricsHistoryReport,
	loaded metricsRollupWindow,
	lastPassAt time.Time,
	pricing adapterruntime.PricingTable,
) {
	report.Restarts = loaded.Restarts
	report.sawCanonicalRecord = loaded.Found || !loaded.EarliestSeen.IsZero()
	report.historyCoversStart = !loaded.EarliestSeen.IsZero() &&
		!loaded.EarliestSeen.After(report.Window.Since)
	report.liveCoversEnd = !lastPassAt.IsZero() &&
		report.Window.Until.Sub(lastPassAt) <= metricsRollupInterval

	if !report.sawCanonicalRecord {
		addMetricsWarning(report, "no distilled daemon history yet")
	} else if !report.historyCoversStart {
		addMetricsWarning(report, "distilled history starts after the requested window")
	}
	if !report.liveCoversEnd {
		if lastPassAt.IsZero() {
			addMetricsWarning(report, "distiller has not completed a pass yet")
		} else {
			addMetricsWarning(report, fmt.Sprintf(
				"distilled history may be up to %s behind; the next distill pass has not run yet",
				report.Window.Until.Sub(lastPassAt).Round(time.Second)))
		}
	}
	if loaded.Restarts > 0 {
		addMetricsWarning(report, fmt.Sprintf("window spans %d daemon restart(s); totals are summed across them", loaded.Restarts))
	}
	aggregateMetricsRequests(report, loaded.Requests, pricing)
	report.Coverage.Complete = report.historyCoversStart && report.liveCoversEnd && !report.invalidHistory
}
