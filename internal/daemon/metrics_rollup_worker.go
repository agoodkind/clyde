package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
)

const (
	// metricsRollupInterval is how often a pass distills the daemon log.
	//
	// Each pass re-reads the active log from one overlap behind its cursor, so
	// a shorter interval repeats that read more often for the same result. A
	// few minutes keeps the widest reported window current while leaving the
	// status command reading a store that is never more than one pass stale.
	metricsRollupInterval = 5 * time.Minute
	// metricsRollupHookName owns the worker's stop in the lifecycle group.
	metricsRollupHookName = "metrics-rollup"
)

// metricsRollupWorker distills finished requests out of the daemon log so the
// status command can report several windows without replaying that log itself.
type metricsRollupWorker struct {
	logPath      string
	rollupPath   string
	interval     time.Duration
	log          *slog.Logger
	now          func() time.Time
	lastRecordAt time.Time
	pricing      adapterruntime.PricingTable
}

// newMetricsRollupWorker builds the worker from the daemon's own logging and
// pricing configuration.
func newMetricsRollupWorker(log *slog.Logger) *metricsRollupWorker {
	if log == nil {
		log = slog.Default()
	}
	logPath := ""
	pricing := adapterruntime.NewPricingTable(nil)
	if cfg, err := config.LoadGlobalOrDefault(); err == nil {
		logPath = slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon)
		pricing = adapterruntime.NewPricingTable(cfg.Adapter.ModelPricing())
	}
	return &metricsRollupWorker{
		logPath:      logPath,
		rollupPath:   metricsRollupPath(),
		interval:     metricsRollupInterval,
		log:          log,
		now:          time.Now,
		lastRecordAt: time.Time{},
		pricing:      pricing,
	}
}

// run marks this daemon generation, then distills on an interval until the
// context is cancelled.
func (w *metricsRollupWorker) run(ctx context.Context) {
	w.recordGeneration(ctx)
	w.lastRecordAt = w.resumeCursor()
	w.runPassAndLog(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runPassAndLog(ctx)
		}
	}
}

// resumeCursor reads how far a previous daemon generation had distilled, so a
// restart neither rewrites records nor leaves a gap behind the cursor.
func (w *metricsRollupWorker) resumeCursor() time.Time {
	checkpoint := readMetricsRollupCheckpoint(metricsRollupCheckpointPath())
	cursor, ok := parseRollupTime(checkpoint.LastRecordAt)
	if !ok {
		return time.Time{}
	}
	return cursor
}

// recordGeneration appends the marker that lets a window report how many
// daemon restarts its totals are summed across.
func (w *metricsRollupWorker) recordGeneration(ctx context.Context) {
	record := generationRollupRecord(w.now())
	if err := appendMetricsRollupRecords(w.rollupPath, []metricsRollupRecord{record}); err != nil {
		w.log.WarnContext(ctx, "daemon.metrics_rollup.generation_write_failed",
			"concern", "daemon.workers",
			"component", "daemon",
			"subcomponent", "metrics_rollup",
			"path", w.rollupPath,
			"err", err.Error(),
		)
	}
}

// runPassAndLog runs one distill pass and contains a panic to that pass, so the
// ticker keeps its cadence and the next pass still happens.
func (w *metricsRollupWorker) runPassAndLog(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.log.ErrorContext(ctx, "daemon.metrics_rollup.panic",
				"concern", "daemon.workers",
				"component", "daemon",
				"subcomponent", "metrics_rollup",
				"err", fmt.Sprintf("panic: %v", recovered),
			)
		}
	}()
	if w.logPath == "" {
		return
	}
	startedAt := w.now()
	result, err := distillMetricsRollup(ctx, metricsRollupDistillInput{
		LogPath:      w.logPath,
		RollupPath:   w.rollupPath,
		Now:          startedAt,
		LastRecordAt: w.lastRecordAt,
		Pricing:      w.pricing,
	})
	if err != nil {
		w.log.WarnContext(ctx, "daemon.metrics_rollup.pass_failed",
			"concern", "daemon.workers",
			"component", "daemon",
			"subcomponent", "metrics_rollup",
			"path", w.rollupPath,
			"err", err.Error(),
		)
		return
	}
	w.lastRecordAt = result.LastRecordAt
	if err := writeMetricsRollupCheckpoint(metricsRollupCheckpointPath(), metricsRollupCheckpoint{
		LastRecordAt: formatRollupTime(result.LastRecordAt),
		LastPassAt:   formatRollupTime(w.now()),
	}); err != nil {
		w.log.WarnContext(ctx, "daemon.metrics_rollup.checkpoint_write_failed",
			"concern", "daemon.workers",
			"component", "daemon",
			"subcomponent", "metrics_rollup",
			"err", err.Error(),
		)
	}
	w.log.DebugContext(ctx, "daemon.metrics_rollup.pass_completed",
		"concern", "daemon.workers",
		"component", "daemon",
		"subcomponent", "metrics_rollup",
		"count", result.Written,
		"pruned", result.Pruned,
		"duration_ms", w.now().Sub(startedAt).Milliseconds(),
	)
}

// installMetricsRollupStop creates the worker context and registers its stop as
// a PhaseWorkers before-hook, so a drain cancels and joins this worker inside
// the workers phase rather than finishing while it still reads the log.
func installMetricsRollupStop(
	ctx context.Context,
	group *livetrack.Group,
	log *slog.Logger,
) (context.Context, chan struct{}, bool) {
	if group == nil {
		return nil, nil, false
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	group.AddHookBefore(livetrack.PhaseWorkers, metricsRollupHookName, func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			log.WarnContext(stopCtx, "daemon.metrics_rollup.stop_timeout",
				"concern", "daemon.workers",
				"component", "daemon",
				"subcomponent", "metrics_rollup",
				"err", stopCtx.Err(),
			)
			return fmt.Errorf("wait for metrics rollup worker: %w", stopCtx.Err())
		}
	})
	return workerCtx, done, true
}

// startMetricsRollup starts the distiller. A nil lifecycle group leaves the
// goroutine unowned across a reload, so nothing starts.
func startMetricsRollup(ctx context.Context, log *slog.Logger, group *livetrack.Group) bool {
	if log == nil {
		log = slog.Default()
	}
	workerCtx, done, owned := installMetricsRollupStop(ctx, group, log)
	if !owned {
		return false
	}
	worker := newMetricsRollupWorker(log)
	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(workerCtx, "daemon.metrics_rollup.worker_panic",
					"concern", "daemon.workers",
					"component", "daemon",
					"subcomponent", "metrics_rollup",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		worker.run(workerCtx)
	}()
	return true
}
