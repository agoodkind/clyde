package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/api/contextpb"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/contextsvc"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitmshow"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/trace"
)

const (
	daemonShutdownTimeout = 5 * time.Second
	reloadDrainCap        = 60 * time.Second
	reloadDrainIdleGrace  = 5 * time.Second

	listenerNameDaemon  = "daemon"
	listenerNameAdapter = "adapter"
	// listenerNameAdapterCursor keys the optional second adapter listener
	// (Cursor BYOK ingress) for reload file-descriptor inheritance.
	listenerNameAdapterCursor = "adapter.cursor"
	// listenerNameMITMPrefix prefixes each per-listener MITM reload key. The
	// daemon keys inherited file descriptors and per-listener proxies by
	// listenerNameMITMPrefix+<listener id> so a reload restores one FD per
	// configured [[mitm.listeners]] block.
	listenerNameMITMPrefix = "mitm:"
	// listenerNamePProf keys the optional loopback pprof listener for reload
	// file-descriptor inheritance, so the debug HTTP surface keeps its open
	// socket across a hot reload instead of colliding with the draining
	// generation on rebind.
	listenerNamePProf = "debug.pprof"
)

// ExtraLoop is a small daemon-owned background loop hook.
type ExtraLoop func(*slog.Logger) func()

// RunCommandContext starts the launchd or systemd-owned supervisor process and
// forwards context cancellation as a process-level termination signal.
func RunCommandContext(ctx context.Context, log *slog.Logger, _ ...ExtraLoop) error {
	if log == nil {
		log = slog.Default()
	}
	if err := config.EnsureRuntimeDir(); err != nil {
		return fmt.Errorf("ensure runtime dir: %w", err)
	}
	stopUpdateScheduler := startSelfUpdateScheduler(ctx, log)
	defer stopUpdateScheduler()
	superviseErr := startSupervisorProcess(ctx, log)
	select {
	case err := <-superviseErr:
		if err != nil {
			log.WarnContext(ctx, "daemon.supervisor.failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
			return fmt.Errorf("run daemon supervisor: %w", err)
		}
		return nil
	case <-ctx.Done():
		err := <-superviseErr
		if err != nil {
			log.WarnContext(ctx, "daemon.supervisor.failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
			return fmt.Errorf("run daemon supervisor: %w", err)
		}
		return nil
	}
}

// RunContext starts the daemon worker process using ctx as the parent context
// for shutdown and child loops.
func RunContext(parent context.Context, log *slog.Logger, extraLoops ...ExtraLoop) (err error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.WarnContext(parent, "daemon.config.load_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return fmt.Errorf("load daemon config: %w", err)
	}
	// Hash the config now, before any reload child could re-trigger on the
	// content it just loaded. The watcher started below skips a change whose
	// hash equals this baseline, so a touch or a reload-child sees no change.
	// ctx is not yet established here; configFileHash logs its own failures.
	configBaselineHash, _ := configFileHash(parent, log, config.GlobalConfigPath())
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	watchSignals(ctx, log, cancel)
	defer trace.Op(ctx, "daemon.worker.run")(&err)

	inherited, err := loadInheritedRuntime()
	if err != nil {
		return fmt.Errorf("load inherited daemon runtime: %w", err)
	}
	stats := newProviderStatsRecorder(adapterruntime.NewPricingTable(cfg.Adapter.ModelPricing()))
	runtime, err := startRuntime(ctx, cfg, log, stats, inherited)
	if err != nil {
		return fmt.Errorf("start daemon runtime: %w", err)
	}
	defer runtime.shutdown(ctx)

	conversationIndex := conversation.NewIndex(newConversationRegistry(), cfg.Conversation)
	semanticFreshness := newConversationSemanticFreshness()

	// Resolve the feeder client per pass rather than once here: when the engine
	// is down at boot the resolver returns nil, and the worker starts anyway and
	// picks the client up once the background registration retry succeeds, with
	// no daemon reload. A nil resolver means semantic search is not configured.
	var resolveSemanticClient conversationSemanticClientResolver
	if runtime.semantic != nil {
		resolveSemanticClient = runtime.semantic.syncClient
	}
	// Start the feeder before the control server serves. Its stop is installed on
	// the lifecycle group inside the start call, ahead of the goroutine launch, so
	// a ReloadDaemon or RebindDaemon RPC cannot begin the workers-phase drain
	// while the feeder is running unowned. Reload and shutdown then both cancel
	// and wait for the feeder before the semantic connection registry drains the
	// engine connection it feeds.
	startConversationSemanticSync(ctx, log, conversationIndex, resolveSemanticClient, cfg.Conversation.Semantic.CollectionID, semanticFreshness, runtime.group)

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(controlMaxMessageBytes),
		grpc.MaxSendMsgSize(controlMaxMessageBytes),
	)
	clydev1.RegisterClydeServiceServer(grpcServer, newControlServer(cfg, log, stats, conversationIndex, semanticFreshness.snapshot, grpcServer, runtime))
	registerConversationContextService(grpcServer, conversationIndex)
	grpcDone := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				grpcDone <- fmt.Errorf("daemon grpc serve panic: %v", recovered)
			}
		}()
		grpcDone <- grpcServer.Serve(runtime.listener)
	}()

	loopStops := startExtraLoops(log, extraLoops)
	defer stopExtraLoops(loopStops)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.conversation_index.panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		conversationIndex.Start(ctx, time.Minute)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.mitm_drift.panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		mitm.RunPeriodicDrift(ctx, runtime.captureStore, cfg.MITM, log)
	}()
	startDebugFacilities(ctx, log)

	if err := signalReady(); err != nil {
		return fmt.Errorf("signal daemon readiness: %w", err)
	}
	log.InfoContext(ctx, "daemon.worker.ready", "concern", "process.daemon.lifecycle", "component", "daemon",
		"adapter_enabled", cfg.Adapter.Enabled,
		"mitm_enabled", cfg.MITM.EnabledDefault,
		"conversation_semantic_enabled", runtime.semantic != nil,
	)
	// Start the config watcher only after readiness so a config edit landing
	// during boot cannot trigger a reload before this worker is the
	// established generation.
	startConfigWatcher(ctx, log, runtime, configBaselineHash)
	startStartupCleanup(log, cfg)

	select {
	case <-ctx.Done():
		grpcServer.GracefulStop()
		return nil
	case err := <-grpcDone:
		runtime.waitReloadDrain(log)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("daemon grpc failed: %w", err)
		}
		return nil
	case err := <-runtime.errors:
		if err != nil {
			return fmt.Errorf("daemon runtime failed: %w", err)
		}
		return nil
	}
}

func registerConversationContextService(grpcServer *grpc.Server, index *conversation.Index) {
	contextpb.RegisterConversationContextServer(grpcServer, contextsvc.New(index))
}

func startSupervisorProcess(ctx context.Context, log *slog.Logger) <-chan error {
	superviseErr := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				superviseErr <- fmt.Errorf("daemon supervisor panic: %v", recovered)
			}
		}()
		superviseErr <- daemonsupervisor.SuperviseContext(ctx, log, config.RuntimeDir())
	}()
	return superviseErr
}

func watchSignals(ctx context.Context, log *slog.Logger, cancel context.CancelFunc) {
	signalCh := make(chan os.Signal, 2)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer func() {
			signal.Stop(signalCh)
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.worker.signal_watch_panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		select {
		case <-signalCh:
			cancel()
		case <-ctx.Done():
		}
	}()
}

// startConfigWatcher constructs and starts the daemon's config-file watcher,
// recording it on the runtime so reload and shutdown can drain it. A start
// failure is non-fatal: the daemon keeps serving without auto-reload.
func startConfigWatcher(ctx context.Context, log *slog.Logger, runtime *runtimeServices, baselineHash string) {
	runtime.configWatcher = newConfigWatcher(log, baselineHash, runtime)
	if err := runtime.configWatcher.start(ctx); err != nil {
		log.WarnContext(ctx, "daemon.config_watch.start_failed", "concern", "process.daemon.config", "component", "daemon", "err", err)
		runtime.configWatcher = nil
	}
}

func startStartupCleanup(log *slog.Logger, cfg *config.Config) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("daemon.log_cleanup.panic", "concern", "process.daemon.lifecycle", "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		runStartupCleanup(log, cfg)
	}()
}

func runStartupCleanup(log *slog.Logger, cfg *config.Config) {
	if cfg == nil {
		return
	}
	setupPolicy, err := logpolicy.ResolveSloggerSetup(*cfg, slogger.ProcessRoleDaemon)
	if err != nil {
		log.Warn("daemon.log_cleanup.policy_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return
	}
	result, err := slogger.RunCleanupOnce(setupPolicy.CleanupPolicy)
	if err != nil {
		log.Warn("daemon.log_cleanup.failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return
	}
	log.Info("daemon.log_cleanup.completed", "concern", "process.daemon.lifecycle", "component", "daemon",
		"scanned_roots", result.ScannedRoots,
		"candidates", result.Candidates,
		"deleted", result.Deleted,
		"bytes_deleted", result.BytesDeleted,
	)
}

// newControlServer builds the gRPC control server, wiring the closures that
// reach back into the daemon runtime for MITM status, capture rendering,
// conversation-index freshness, and reload.
func newControlServer(
	cfg *config.Config,
	log *slog.Logger,
	stats *providerStatsRecorder,
	index *conversation.Index,
	freshness func() conversation.SearchFreshness,
	grpcServer *grpc.Server,
	runtime *runtimeServices,
) *controlServer {
	semanticSearch := func() conversationSemanticSearchClient {
		if !cfg.Conversation.Semantic.Enabled || runtime.semantic == nil {
			return nil
		}
		return runtime.semantic.currentSearchClient()
	}
	searchSource := &semanticConversationSearchSource{
		index:        index,
		searchClient: semanticSearch,
		collectionID: cfg.Conversation.Semantic.CollectionID,
	}
	return &controlServer{
		UnimplementedClydeServiceServer: clydev1.UnimplementedClydeServiceServer{},
		stats:                           stats,
		index:                           index,
		loggingConfig:                   cfg.Logging,
		mitmConfig:                      cfg.MITM,
		mitmStatus: func() MITMStatus {
			return collectMITMStatus(cfg.MITM, runtime.mitmListeners)
		},
		showCapture: func(showCtx context.Context, id string) (mitmshow.ShowOutput, error) {
			return mitmshow.Lookup(showCtx, cfg, id)
		},
		reload: func(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
			return reloadDaemonWorker(ctx, log, grpcServer, runtime)
		},
		rebind: func(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
			return rebindDaemonWorker(ctx, log, grpcServer, runtime)
		},
		searchSource: searchSource,
		captureStore: runtime.captureStore,
		freshness:    freshness,
		exportTokens: newExportTokenConfig(cfg),
	}
}

func startExtraLoops(log *slog.Logger, loops []ExtraLoop) []func() {
	stops := make([]func(), 0, len(loops))
	for _, loop := range loops {
		if loop == nil {
			continue
		}
		stop := loop(log)
		if stop != nil {
			stops = append(stops, stop)
		}
	}
	return stops
}

func stopExtraLoops(stops []func()) {
	for _, stop := range slices.Backward(stops) {
		stop()
	}
}

func signalReady() error {
	fdText := strings.TrimSpace(os.Getenv(daemonsupervisor.EnvReadyFD))
	if fdText == "" {
		return nil
	}
	fd, err := strconv.Atoi(fdText)
	if err != nil {
		slog.Warn("daemon.ready_fd.parse_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "fd", fdText, "err", err)
		return fmt.Errorf("parse daemon ready fd %q: %w", fdText, err)
	}
	file := os.NewFile(uintptr(fd), "clyde-daemon-ready")
	if file == nil {
		return fmt.Errorf("open daemon ready fd %d", fd)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString("ready\n"); err != nil {
		slog.Warn("daemon.ready_fd.write_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "fd", fd, "err", err)
		return fmt.Errorf("write daemon ready fd: %w", err)
	}
	return nil
}

func normalizeListenHost(host string) string {
	trimmed := strings.TrimSpace(host)
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	return trimmed
}

func errNoSupervisorSocket(path string) error {
	return fmt.Errorf("supervisor socket is unavailable at %s", path)
}

func socketExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func ensureScratchDir() string {
	path := filepath.Join(config.RuntimeDir(), "adapter-scratch")
	if err := os.MkdirAll(path, 0o700); err != nil {
		return ""
	}
	return path
}
