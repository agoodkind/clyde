package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/slogger"
)

const storageRetentionInterval = time.Second

func startStorageRetention(ctx context.Context, log *slog.Logger, runtime *runtimeServices, metrics *metricsRollupWorker) {
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan bool)
	runtime.group.AddHookBefore(livetrack.PhaseWorkers, "storage.retention", func(stopContext context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopContext.Done():
			return fmt.Errorf("wait for storage retention: %w", stopContext.Err())
		}
	})
	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(workerContext, "daemon.retention.panic", "err", recovered)
			}
		}()
		if err := awaitRetentionOwnership(workerContext, log); err != nil {
			if workerContext.Err() == nil {
				log.WarnContext(workerContext, "daemon.retention.ownership_failed", "err", err)
			}
			return
		}
		var workers sync.WaitGroup
		if runtime.captureStore != nil {
			workers.Go(func() {
				runRetentionWorker(workerContext, log, func() { runtime.captureStore.RunRetention(workerContext) })
			})
		}
		workers.Go(func() {
			runRetentionWorker(workerContext, log, func() { runLogRetention(workerContext, log, runtime, metrics) })
		})
		workers.Wait()
	}()
}

func runRetentionWorker(ctx context.Context, log *slog.Logger, operation func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.ErrorContext(ctx, "daemon.retention.worker_panic", "err", recovered)
		}
	}()
	operation()
}

func awaitRetentionOwnership(ctx context.Context, log *slog.Logger) error {
	socket := os.Getenv(daemonsupervisor.EnvSupervisorSocket)
	if socket == "" {
		return nil
	}
	ticker := time.NewTicker(storageRetentionInterval)
	defer ticker.Stop()
	for {
		status, err := daemonsupervisor.RequestStatus(ctx, socket)
		if err == nil {
			children, listError := childProcessIDs(ctx, status.PID)
			if listError == nil && len(children) == 1 && children[0] == os.Getpid() {
				return nil
			}
			if listError != nil {
				log.WarnContext(ctx, "daemon.retention.predecessor_query_failed", "err", listError)
			}
		} else if ctx.Err() == nil {
			log.WarnContext(ctx, "daemon.retention.supervisor_query_failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for retention ownership: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func runLogRetention(ctx context.Context, log *slog.Logger, runtime *runtimeServices, metrics *metricsRollupWorker) {
	ticker := time.NewTicker(storageRetentionInterval)
	defer ticker.Stop()
	for {
		if err := retainDaemonLogs(ctx, runtime.currentConfig.Load(), metrics); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "daemon.log_cleanup.failed", "concern", "daemon.workers", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func retainDaemonLogs(ctx context.Context, cfg *config.Config, metrics *metricsRollupWorker) error {
	setup, err := logpolicy.ResolveSloggerSetup(*cfg, slogger.ProcessRoleDaemon)
	if err != nil {
		slog.WarnContext(ctx, "daemon.retention.log_policy_failed", "err", err)
		return fmt.Errorf("resolve log retention: %w", err)
	}
	if !setup.CleanupPolicy.Enabled {
		return nil
	}
	stateDirectory := config.DefaultStateDir()
	setup.CleanupPolicy.Root = setup.ConcernRoot
	setup.CleanupPolicy.Paths = append(setup.CleanupPolicy.Paths,
		slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleCLI),
		slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon),
		anthropic.LogPath(), adaptercodex.LogPath(),
		filepath.Join(stateDirectory, "audit.jsonl"),
		filepath.Join(stateDirectory, "daemon.log"),
		metricsRollupPath(),
	)
	setup.CleanupPolicy.Roots = append(setup.CleanupPolicy.Roots,
		slogger.DefaultConcernRoot(cfg.Logging, slogger.ProcessRoleCLI),
		filepath.Join(cfg.MITM.CaptureDir, "raw"),
		filepath.Join(cfg.MITM.CaptureDir, "drift"),
		filepath.Join(stateDirectory, "mitm-drift"),
	)
	for _, spec := range config.LoggingSinkSpecs() {
		if override, ok := cfg.Logging.Sinks.Override(spec.Name); ok && override.Path != "" {
			setup.CleanupPolicy.Paths = append(setup.CleanupPolicy.Paths, override.Path)
		}
	}
	setup.CleanupPolicy, err = supervisorRetentionPaths(ctx, setup.CleanupPolicy)
	if err != nil {
		return err
	}
	if err := metrics.prepareCleanup(ctx, false); err != nil {
		return err
	}
	result, cleanupError := slogger.RunCleanupOnceContext(ctx, setup.CleanupPolicy)
	changed := append([]string(nil), result.TrimmedPaths...)
	changed = append(changed, result.DeletedPaths...)
	processPath := slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon)
	if slices.Contains(changed, filepath.Clean(processPath)) || slices.Contains(changed, metricsRollupPath()) {
		if err := metrics.prepareCleanup(ctx, true); err != nil {
			slog.WarnContext(ctx, "daemon.retention.metrics_reset_failed", "err", err)
			return fmt.Errorf("reset metrics coverage after retention: %w", err)
		}
	}
	if cleanupError != nil {
		slog.WarnContext(ctx, "daemon.retention.logs_failed", "err", cleanupError)
		return fmt.Errorf("retain daemon logs: %w", cleanupError)
	}
	return nil
}
