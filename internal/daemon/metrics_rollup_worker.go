package daemon

import (
	"context"
	"errors"
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
	metricsRollupInterval = 5 * time.Minute
	// metricsRollupHookName owns the worker's stop in the lifecycle group.
	metricsRollupHookName = "metrics-rollup"
)

// metricsRollupWorker distills finished requests out of the daemon log so the
// status command can report several windows without replaying that log itself.
type metricsRollupWorker struct {
	cleanup    chan metricsCleanupRequest
	logPath    string
	rollupPath string
	interval   time.Duration
	log        *slog.Logger
	now        func() time.Time
	state      metricsRollupState
	pricing    adapterruntime.PricingTable
}

type metricsCleanupRequest struct {
	changed bool
	done    chan error
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
	var state metricsRollupState
	return &metricsRollupWorker{
		cleanup:    make(chan metricsCleanupRequest),
		logPath:    logPath,
		rollupPath: metricsRollupPath(),
		interval:   metricsRollupInterval,
		log:        log,
		now:        time.Now,
		state:      state,
		pricing:    pricing,
	}
}

// run marks this daemon generation, then distills on an interval until the
// context is cancelled.
func (w *metricsRollupWorker) run(ctx context.Context) {
	defer func() {
		if err := w.state.closeSource(); err != nil {
			w.log.WarnContext(ctx, "daemon.metrics_rollup.source_close_failed", "path", w.logPath, "err", err)
		}
	}()
	w.recordGeneration(ctx)
	w.state.checkpoint = readMetricsRollupCheckpoint(metricsRollupCheckpointPath())
	w.state.saved = w.state.checkpoint
	_ = w.runPassAndLog(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = w.runPassAndLog(ctx)
		case request := <-w.cleanup:
			var cleanupError error
			if request.changed {
				cleanupError = w.state.closeSource()
				if cleanupError != nil {
					w.log.WarnContext(ctx, "daemon.metrics_rollup.cleanup_close_failed", "err", cleanupError)
				}
				var position metricsRollupSourcePosition
				w.state.checkpoint.Source = position
				w.state.checkpoint.CoverageSince = formatRollupTime(w.now())
				cleanupError = errors.Join(cleanupError, w.writeCheckpoint(ctx))
			} else {
				cleanupError = w.runPassAndLog(ctx)
			}
			request.done <- cleanupError
		}
	}
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
func (w *metricsRollupWorker) runPassAndLog(ctx context.Context) (passError error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			passError = fmt.Errorf("metrics rollup panic: %v", recovered)
			w.log.ErrorContext(ctx, "daemon.metrics_rollup.panic",
				"concern", "daemon.workers",
				"component", "daemon",
				"subcomponent", "metrics_rollup",
				"err", fmt.Sprintf("panic: %v", recovered),
			)
		}
	}()
	if w.logPath == "" {
		return errors.New("metrics rollup requires a daemon log path")
	}
	startedAt := w.now()
	result, err := distillMetricsRollup(ctx, metricsRollupDistillInput{
		LogPath:    w.logPath,
		RollupPath: w.rollupPath,
		Now:        startedAt,
		State:      &w.state,
		Pricing:    w.pricing,
	})
	if err != nil {
		w.log.WarnContext(ctx, "daemon.metrics_rollup.pass_failed",
			"concern", "daemon.workers",
			"component", "daemon",
			"subcomponent", "metrics_rollup",
			"path", w.rollupPath,
			"err", err.Error(),
		)
		return err
	}
	if w.state.checkpointChanged() {
		if err := w.writeCheckpoint(ctx); err != nil {
			return err
		}
	}
	w.log.DebugContext(ctx, "daemon.metrics_rollup.pass_completed",
		"concern", "daemon.workers",
		"component", "daemon",
		"subcomponent", "metrics_rollup",
		"count", result.Written,
		"pruned", result.Pruned,
		"bytes_read", result.BytesRead,
		"duration_ms", w.now().Sub(startedAt).Milliseconds(),
	)
	return nil
}

func (w *metricsRollupWorker) writeCheckpoint(ctx context.Context) error {
	w.state.checkpoint.LastPassAt = formatRollupTime(w.now())
	if err := writeMetricsRollupCheckpoint(metricsRollupCheckpointPath(), w.state.checkpoint); err != nil {
		w.log.WarnContext(ctx, "daemon.metrics_rollup.checkpoint_write_failed",
			"concern", "daemon.workers",
			"component", "daemon",
			"subcomponent", "metrics_rollup",
			"err", err.Error(),
		)
		return err
	}
	w.state.saved = w.state.checkpoint
	return nil
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

// StartMetricsRollup starts the distiller. A nil lifecycle group leaves the
// goroutine unowned across a reload, so nothing starts.
func StartMetricsRollup(ctx context.Context, log *slog.Logger, group *livetrack.Group) bool {
	return startMetricsRollupWorker(ctx, log, group) != nil
}

func startMetricsRollupWorker(ctx context.Context, log *slog.Logger, group *livetrack.Group) *metricsRollupWorker {
	if log == nil {
		log = slog.Default()
	}
	workerCtx, done, owned := installMetricsRollupStop(ctx, group, log)
	if !owned {
		return nil
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
	return worker
}

func (w *metricsRollupWorker) prepareCleanup(ctx context.Context, changed bool) error {
	if w == nil {
		return nil
	}
	request := metricsCleanupRequest{changed: changed, done: make(chan error, 1)}
	select {
	case w.cleanup <- request:
	case <-ctx.Done():
		w.log.WarnContext(ctx, "daemon.metrics_rollup.cleanup_notify_canceled", "err", ctx.Err())
		return fmt.Errorf("notify metrics retention: %w", ctx.Err())
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		w.log.WarnContext(ctx, "daemon.metrics_rollup.cleanup_wait_canceled", "err", ctx.Err())
		return fmt.Errorf("wait for metrics retention: %w", ctx.Err())
	}
}
