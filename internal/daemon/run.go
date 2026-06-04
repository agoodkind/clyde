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
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
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

// RunCommand starts the launchd or systemd-owned supervisor process.
func RunCommand(log *slog.Logger, _ ...ExtraLoop) error {
	if log == nil {
		log = slog.Default()
	}
	if err := config.EnsureRuntimeDir(); err != nil {
		return fmt.Errorf("ensure runtime dir: %w", err)
	}
	if err := daemonsupervisor.Supervise(log, config.RuntimeDir()); err != nil {
		log.Warn("daemon.supervisor.failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return fmt.Errorf("run daemon supervisor: %w", err)
	}
	return nil
}

// Run starts the daemon worker process.
func Run(log *slog.Logger, extraLoops ...ExtraLoop) (err error) {
	if log == nil {
		log = slog.Default()
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.Warn("daemon.config.load_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return fmt.Errorf("load daemon config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer trace.Op(ctx, "daemon.worker.run")(&err)

	inherited, err := loadInheritedRuntime()
	if err != nil {
		return fmt.Errorf("load inherited daemon runtime: %w", err)
	}
	stats := newProviderStatsRecorder(adapterruntime.NewPricingTable(cfg.Adapter.Pricing))
	runtime, err := startRuntime(ctx, cfg, log, stats, inherited)
	if err != nil {
		return fmt.Errorf("start daemon runtime: %w", err)
	}
	defer runtime.shutdown(context.Background())

	conversationIndex := conversation.NewIndex(newConversationRegistry())

	grpcServer := grpc.NewServer()
	clydev1.RegisterClydeServiceServer(grpcServer, &controlServer{
		UnimplementedClydeServiceServer: clydev1.UnimplementedClydeServiceServer{},
		stats:                           stats,
		index:                           conversationIndex,
		searchConfig:                    cfg.Search,
		loggingConfig:                   cfg.Logging,
		mitmConfig:                      cfg.MITM,
		mitmStatus: func() MITMStatus {
			return collectMITMStatus(cfg.MITM, runtime.mitmListeners)
		},
		showCapture: func(showCtx context.Context, id string, asJSON bool) (string, error) {
			return mitmshow.Render(showCtx, cfg, id, asJSON)
		},
		reload: func(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
			return reloadDaemonWorker(ctx, log, grpcServer, runtime)
		},
	})
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
				log.Error("daemon.conversation_index.panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		conversationIndex.Start(ctx, time.Minute)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("daemon.mitm_drift.panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		mitm.RunPeriodicDrift(ctx, cfg.MITM, log)
	}()
	startDebugFacilities(ctx, log)

	if err := signalReady(); err != nil {
		return fmt.Errorf("signal daemon readiness: %w", err)
	}
	log.Info("daemon.worker.ready", "concern", "process.daemon.lifecycle", "component", "daemon",
		"adapter_enabled", cfg.Adapter.Enabled,
		"mitm_enabled", cfg.MITM.EnabledDefault,
	)
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

type runtimeServices struct {
	listener              net.Listener
	adapter               *adapter.Server
	adapterListener       net.Listener
	adapterCursorListener net.Listener
	// mitmProxies holds one running proxy per configured MITM listener,
	// keyed by listener id. mitmListeners holds the matching bound sockets
	// keyed by the same id; a "localhost" listener binds two loopback sockets
	// ([::1] and 127.0.0.1) served by the one proxy, so the slice has two
	// entries. captureStore is the single SQLite capture sink shared by every
	// proxy; it is closed after the proxies drain so the WAL flushes before any
	// new generation reopens it.
	mitmProxies   map[string]*mitm.Proxy
	mitmListeners map[string][]net.Listener
	captureStore  *capture.Store
	// pprofListener is the optional loopback pprof socket. It is nil when pprof
	// is off. When set, it is inherited across reload like the adapter and MITM
	// listeners so the debug surface survives a hot reload with no bind gap.
	pprofListener net.Listener
	errors        chan error
	reloadMu      sync.Mutex
	reloadDrain   <-chan struct{}
}

type inheritedRuntime struct {
	listeners map[string]net.Listener
	ready     *os.File
}

func startRuntime(
	ctx context.Context,
	cfg *config.Config,
	log *slog.Logger,
	stats *providerStatsRecorder,
	inherited inheritedRuntime,
) (*runtimeServices, error) {
	listener, err := daemonListener(ctx, config.DaemonSocketPath(), inherited.listeners[listenerNameDaemon])
	if err != nil {
		return nil, err
	}
	runtime := &runtimeServices{
		listener:              listener,
		adapter:               nil,
		adapterListener:       nil,
		adapterCursorListener: nil,
		mitmProxies:           map[string]*mitm.Proxy{},
		mitmListeners:         map[string][]net.Listener{},
		captureStore:          nil,
		pprofListener:         nil,
		errors:                make(chan error, 3),
		reloadMu:              sync.Mutex{},
		reloadDrain:           nil,
	}
	if cfg.MITM.EnabledDefault {
		if err := startMITM(ctx, cfg, log, runtime, inherited.listeners); err != nil {
			runtime.shutdown(context.WithoutCancel(ctx))
			return nil, err
		}
	} else if hasInheritedMITMListener(inherited.listeners) {
		runtime.shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("mitm listener inherited but mitm is disabled; full daemon restart required")
	}
	if cfg.Adapter.Enabled {
		server, adapterListener, adapterCursorListener, err := startAdapter(ctx, cfg, log, stats, runtime.captureStore, runtime.errors, inherited.listeners[listenerNameAdapter], inherited.listeners[listenerNameAdapterCursor])
		if err != nil {
			runtime.shutdown(context.WithoutCancel(ctx))
			return nil, err
		}
		runtime.adapter = server
		runtime.adapterListener = adapterListener
		runtime.adapterCursorListener = adapterCursorListener
	} else if inherited.listeners[listenerNameAdapter] != nil || inherited.listeners[listenerNameAdapterCursor] != nil {
		runtime.shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("adapter listener inherited but adapter is disabled; full daemon restart required")
	}
	if pprofAddr := resolvePProfAddr(cfg.Debug.PProfAddr); pprofAddr != "" {
		listener, err := startPProf(ctx, log, pprofAddr, inherited.listeners[listenerNamePProf])
		if err != nil {
			runtime.shutdown(context.WithoutCancel(ctx))
			return nil, err
		}
		runtime.pprofListener = listener
	} else if inherited.listeners[listenerNamePProf] != nil {
		runtime.shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("pprof listener inherited but pprof is disabled; full daemon restart required")
	}
	return runtime, nil
}

// startMITM opens the single shared capture store and starts one proxy per
// configured MITM listener, populating runtime.captureStore, runtime.mitmProxies,
// and runtime.mitmListeners. On any failure the caller drains the runtime, which
// closes whatever proxies and the store were already started.
func startMITM(ctx context.Context, cfg *config.Config, log *slog.Logger, runtime *runtimeServices, inherited map[string]net.Listener) error {
	store, err := openMITMCaptureStore(ctx, cfg, log)
	if err != nil {
		log.WarnContext(ctx, "daemon.mitm.capture_store_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"db_path", cfg.MITM.CaptureStore.DBPath,
			"err", err,
		)
		return err
	}
	runtime.captureStore = store
	for id, listenerCfg := range cfg.MITM.Listeners {
		listenerCfg.ID = id
		if err := startMITMListener(ctx, cfg, log, runtime, listenerCfg, inherited); err != nil {
			return err
		}
	}
	return nil
}

// startMITMListener binds (or validates the inherited FDs for) every loopback
// socket of one MITM listener, constructs the single proxy that serves them all
// against the shared capture store tagged with the listener id, starts serving,
// and records the proxy and its sockets under the listener id in runtime. A
// "localhost" listener has two sockets ([::1] and 127.0.0.1); an explicit-IP
// listener has one.
func startMITMListener(ctx context.Context, cfg *config.Config, log *slog.Logger, runtime *runtimeServices, listenerCfg config.MITMListenerConfig, inherited map[string]net.Listener) error {
	addrs := mitmBindAddrs(listenerCfg.Host, listenerCfg.Port)
	sockets := make([]net.Listener, 0, len(addrs))
	closeSockets := func() {
		for _, socket := range sockets {
			_ = socket.Close()
		}
	}
	for _, addr := range addrs {
		if existing := inherited[mitmSocketKey(listenerCfg.ID, addr)]; existing != nil {
			if got := existing.Addr().String(); got != addr {
				closeSockets()
				return fmt.Errorf("mitm listener %q inherited address %s does not match config %s; full daemon restart required", listenerCfg.ID, got, addr)
			}
			sockets = append(sockets, existing)
			continue
		}
		listenConfig := net.ListenConfig{}
		bound, err := listenConfig.Listen(ctx, "tcp", addr)
		if err != nil {
			closeSockets()
			log.WarnContext(ctx, "daemon.mitm.listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"listener_id", listenerCfg.ID,
				"addr", addr,
				"err", err,
			)
			return fmt.Errorf("mitm listen %s for listener %q: %w", addr, listenerCfg.ID, err)
		}
		sockets = append(sockets, bound)
	}
	proxy, err := mitm.NewProxy(cfg.MITM, cfg.Logging.Request, log, sockets, runtime.captureStore, listenerCfg.ID)
	if err != nil {
		closeSockets()
		log.WarnContext(ctx, "daemon.mitm.init_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"listener_id", listenerCfg.ID,
			"addrs", addrs,
			"err", err,
		)
		return fmt.Errorf("init mitm proxy for listener %q: %w", listenerCfg.ID, err)
	}
	runtime.mitmProxies[listenerCfg.ID] = proxy
	runtime.mitmListeners[listenerCfg.ID] = sockets
	errCh := runtime.errors
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.mitm.serve_panic", "concern", "process.daemon.lifecycle", "component", "daemon",
					"listener_id", listenerCfg.ID,
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		if serveErr := proxy.Serve(); serveErr != nil {
			errCh <- serveErr
		}
	}()
	log.InfoContext(ctx, "daemon.mitm.started", "concern", "process.daemon.lifecycle", "component", "daemon",
		"listener_id", listenerCfg.ID,
		"addrs", addrs,
	)
	return nil
}

// hasInheritedMITMListener reports whether any inherited listener key belongs to
// a MITM listener, used to reject a reload that disables MITM while a previous
// generation's MITM file descriptors are still being inherited.
func hasInheritedMITMListener(inherited map[string]net.Listener) bool {
	for name := range inherited {
		if strings.HasPrefix(name, listenerNameMITMPrefix) {
			return true
		}
	}
	return false
}

func startAdapter(
	ctx context.Context,
	cfg *config.Config,
	log *slog.Logger,
	stats *providerStatsRecorder,
	store *capture.Store,
	errCh chan<- error,
	inherited net.Listener,
	inheritedCursor net.Listener,
) (*adapter.Server, net.Listener, net.Listener, error) {
	deps := adapter.Deps{
		ScratchDir:                ensureScratchDir,
		RequestEvents:             stats.record,
		RuntimeLogging:            adapter.NewRuntimeLogging(cfg.Logging),
		GetAuth:                   getAuth(cfg, log),
		AnthropicWireBaselinePath: mitmAnthropicWireBaselinePath(cfg),
		CodexWireBaselinePath:     mitmCodexWireBaselinePath(cfg),
		CaptureStore:              store,
	}
	server, err := adapter.New(ctx, cfg.Adapter, cfg.Logging, deps, log)
	if err != nil {
		log.WarnContext(ctx, "daemon.adapter.init_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return nil, nil, nil, fmt.Errorf("adapter init: %w", err)
	}
	primary, err := bindOrInheritTCPListener(ctx, log, server.Addr(), inherited, listenerNameAdapter)
	if err != nil {
		return nil, nil, nil, err
	}
	var cursor net.Listener
	if cursorAddr := server.CursorIngressAddr(); cursorAddr != "" {
		cursor, err = bindOrInheritTCPListener(ctx, log, cursorAddr, inheritedCursor, listenerNameAdapterCursor)
		if err != nil {
			_ = primary.Close()
			return nil, nil, nil, err
		}
	}
	listeners := []net.Listener{primary}
	if cursor != nil {
		listeners = append(listeners, cursor)
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.adapter.serve_panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		if serveErr := server.StartOnListeners(ctx, listeners...); serveErr != nil {
			errCh <- serveErr
		}
	}()
	log.InfoContext(ctx, "daemon.adapter.started", "concern", "process.daemon.lifecycle", "component", "daemon", "addr", primary.Addr().String())
	return server, primary, cursor, nil
}

// bindOrInheritTCPListener returns the inherited listener when its address
// matches addr (reload reuses the open socket with no bind gap), rejecting a
// changed address with a restart-required error, and otherwise binds a fresh
// listener. label names the surface in the log event and error so each caller
// (adapter, adapter cursor, pprof) stays distinguishable.
func bindOrInheritTCPListener(ctx context.Context, log *slog.Logger, addr string, inherited net.Listener, label string) (net.Listener, error) {
	if inherited != nil {
		if got := inherited.Addr().String(); got != addr {
			return nil, fmt.Errorf("%s inherited listener address %s does not match config %s; full daemon restart required", label, got, addr)
		}
		return inherited, nil
	}
	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(ctx, "tcp", addr)
	if err != nil {
		log.WarnContext(ctx, "daemon.listener.listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "surface", label, "addr", addr, "err", err)
		return nil, fmt.Errorf("%s listen %s: %w", label, addr, err)
	}
	return listener, nil
}

// startPProf binds, or inherits across reload, the loopback pprof listener at
// addr and serves net/http/pprof on it in a recovered goroutine. Inheriting the
// open socket from the previous generation removes the rebind race where the
// draining worker still holds the port, so the debug surface stays reachable
// through a hot reload.
func startPProf(ctx context.Context, log *slog.Logger, addr string, inherited net.Listener) (net.Listener, error) {
	listener, err := bindOrInheritTCPListener(ctx, log, addr, inherited, listenerNamePProf)
	if err != nil {
		return nil, err
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.debug.pprof_panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		servePProf(ctx, log, listener)
	}()
	return listener, nil
}

func getAuth(cfg *config.Config, log *slog.Logger) func(adapterresolver.ProviderID) adapterprovider.AuthLookup {
	return func(provider adapterresolver.ProviderID) adapterprovider.AuthLookup {
		switch provider {
		case adapterresolver.ProviderUnknown:
			return nil
		case adapterresolver.ProviderAnthropic:
			return anthropic.NewAuthManager(cfg.Adapter.Anthropic.OAuth, "")
		case adapterresolver.ProviderCodex:
			return adaptercodex.NewAuthManager(cfg.Adapter.Codex.AuthFile, adaptercodex.AuthManagerOptions{
				HTTPClient: nil,
				Log:        log.With("component", "adapter", "subcomponent", "codex_auth"),
				Now:        nil,
				RefreshURL: "",
			})
		default:
			return nil
		}
	}
}

// claudeCodeUpstream is the upstream key the MITM drift config and the
// baseline directory use for the claude-cli wire reference.
const claudeCodeUpstream = "claude-code"

// codexCLIUpstream is the upstream key the MITM drift config and the
// baseline directory use for the codex-cli wire reference. It matches
// the drift writer's driftUpstreamCodexCLI value so the same baseline
// file feeds the codex egress.
const codexCLIUpstream = "codex-cli"

// mitmAnthropicWireBaselinePath resolves the absolute path the Anthropic
// client reads to project its outbound wire identity. It prefers the
// configured [mitm].drift.upstreams["claude-code"].reference and falls
// back to the default baseline root. There is no compiled-in flavor
// data; the adapter fails with an operator-actionable HTTP 503 when the
// file is missing or invalid.
func mitmAnthropicWireBaselinePath(cfg *config.Config) string {
	if cfg == nil {
		return mitm.ResolveWireBaselinePath(claudeCodeUpstream, "")
	}
	configured := ""
	if up, ok := cfg.MITM.Drift.Upstreams[claudeCodeUpstream]; ok {
		configured = up.Reference
	}
	return mitm.ResolveWireBaselinePath(claudeCodeUpstream, configured)
}

// mitmCodexWireBaselinePath resolves the absolute path the Codex
// provider reads to project its outbound wire identity. It prefers the
// configured [mitm].drift.upstreams["codex-cli"].reference and falls
// back to the default baseline root. Unlike the Anthropic resolver, the
// codex egress treats a missing or invalid file as a soft fall-back to
// its compiled-in identity constants rather than an HTTP 503, so a
// cold-start codex works before any baseline has been learned.
func mitmCodexWireBaselinePath(cfg *config.Config) string {
	if cfg == nil {
		return mitm.ResolveWireBaselinePath(codexCLIUpstream, "")
	}
	configured := ""
	if up, ok := cfg.MITM.Drift.Upstreams[codexCLIUpstream]; ok {
		configured = up.Reference
	}
	return mitm.ResolveWireBaselinePath(codexCLIUpstream, configured)
}

func reloadDaemonWorker(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, runtime *runtimeServices) (*clydev1.ReloadDaemonResponse, error) {
	if runtime == nil {
		return nil, fmt.Errorf("daemon runtime is unavailable")
	}
	runtime.reloadMu.Lock()
	defer runtime.reloadMu.Unlock()

	executablePath, err := os.Executable()
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.executable_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.executable_abs_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"path", executablePath,
			"err", err,
		)
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	files, specs, cleanup, err := inheritedListenerFiles(runtime)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.ready_pipe_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("create reload readiness pipe: %w", err)
	}
	defer func() { _ = readyRead.Close() }()
	defer func() { _ = readyWrite.Close() }()
	readyFD := 3 + len(files)
	requestFiles := append([]*os.File{}, files...)
	requestFiles = append(requestFiles, readyWrite)
	pid, err := daemonsupervisor.RequestReplacement(
		ctx,
		daemonsupervisor.SocketPath(config.RuntimeDir()),
		executablePath,
		specs,
		readyFD,
		os.Environ(),
		requestFiles,
	)
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.supervisor_request_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"path", executablePath,
			"err", err,
		)
		return nil, fmt.Errorf("request supervisor replacement daemon: %w", err)
	}
	_ = readyWrite.Close()
	if err := waitForReplacementDaemon(ctx, readyRead); err != nil {
		return nil, err
	}
	// The replacement generation has inherited the pprof socket's file
	// descriptor, so closing this generation's copy stops it serving pprof
	// without dropping the socket, leaving exactly the new worker on the debug
	// port.
	if runtime.pprofListener != nil {
		_ = runtime.pprofListener.Close()
	}
	runtime.reloadDrain = runtime.startReloadDrain(ctx, log)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.reload.grpc_stop_panic", "concern", "daemon.workers.reload", "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		grpcServer.GracefulStop()
	}()
	return &clydev1.ReloadDaemonResponse{
		BinaryReloaded: true,
		ActiveSurfaces: int64(runtime.activeSurfaceCount()),
		NewPid:         int64(pid),
	}, nil
}

func inheritedListenerFiles(runtime *runtimeServices) ([]*os.File, []daemonsupervisor.ListenerSpec, func(), error) {
	if runtime == nil || runtime.listener == nil {
		return nil, nil, func() {}, fmt.Errorf("daemon listener is not available for reload")
	}
	if err := validateReloadListenerConfig(runtime); err != nil {
		return nil, nil, func() {}, err
	}
	type namedListener struct {
		name string
		lis  net.Listener
	}
	listeners := []namedListener{{name: listenerNameDaemon, lis: runtime.listener}}
	if runtime.adapterListener != nil {
		listeners = append(listeners, namedListener{name: listenerNameAdapter, lis: runtime.adapterListener})
	}
	if runtime.adapterCursorListener != nil {
		listeners = append(listeners, namedListener{name: listenerNameAdapterCursor, lis: runtime.adapterCursorListener})
	}
	if runtime.pprofListener != nil {
		listeners = append(listeners, namedListener{name: listenerNamePProf, lis: runtime.pprofListener})
	}
	mitmIDs := make([]string, 0, len(runtime.mitmListeners))
	for id := range runtime.mitmListeners {
		mitmIDs = append(mitmIDs, id)
	}
	slices.Sort(mitmIDs)
	for _, id := range mitmIDs {
		for _, socket := range runtime.mitmListeners[id] {
			listeners = append(listeners, namedListener{name: mitmSocketKey(id, socket.Addr().String()), lis: socket})
		}
	}
	files := make([]*os.File, 0, len(listeners))
	specs := make([]daemonsupervisor.ListenerSpec, 0, len(listeners))
	cleanup := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	for _, named := range listeners {
		file, err := listenerFile(named.lis)
		if err != nil {
			cleanup()
			slog.Warn("daemon.reload.listener_file_failed", "concern", "daemon.workers.reload", "component", "daemon",
				"name", named.name,
				"err", err,
			)
			return nil, nil, func() {}, fmt.Errorf("inherit listener %s: %w", named.name, err)
		}
		files = append(files, file)
		specs = append(specs, daemonsupervisor.ListenerSpec{
			Name:    named.name,
			Network: named.lis.Addr().Network(),
			Addr:    named.lis.Addr().String(),
			FD:      3 + len(files) - 1,
		})
	}
	return files, specs, cleanup, nil
}

func (r *runtimeServices) startReloadDrain(parent context.Context, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(parent, "daemon.reload.public_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), reloadDrainCap)
		defer cancel()
		var wg sync.WaitGroup
		if r.adapter != nil {
			wg.Go(func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						log.ErrorContext(parent, "daemon.reload.adapter_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
							"err", fmt.Sprintf("panic: %v", recovered),
						)
					}
				}()
				if err := r.adapter.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace}); err != nil {
					log.WarnContext(ctx, "daemon.reload.adapter_drain_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
				}
			})
		}
		for id, proxy := range r.mitmProxies {
			wg.Go(func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						log.ErrorContext(parent, "daemon.reload.mitm_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
							"listener_id", id,
							"err", fmt.Sprintf("panic: %v", recovered),
						)
					}
				}()
				if err := proxy.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace}); err != nil {
					log.WarnContext(ctx, "daemon.reload.mitm_drain_failed", "concern", "daemon.workers.reload", "component", "daemon", "listener_id", id, "err", err)
				}
			})
		}
		wg.Wait()
		// Close the shared capture store only after every proxy has drained so
		// the SQLite WAL flushes before the new generation reopens the same db.
		if r.captureStore != nil {
			if err := r.captureStore.Close(parent, "reload"); err != nil {
				log.WarnContext(ctx, "daemon.reload.capture_store_close_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
			}
		}
	}()
	return done
}

func (r *runtimeServices) waitReloadDrain(log *slog.Logger) {
	if r == nil {
		return
	}
	r.reloadMu.Lock()
	done := r.reloadDrain
	r.reloadMu.Unlock()
	if done == nil {
		return
	}
	log.Info("daemon.reload.waiting_for_public_drains", "concern", "daemon.workers.reload", "component", "daemon",
		"cap", reloadDrainCap.String(),
	)
	<-done
}

func (r *runtimeServices) activeSurfaceCount() int {
	count := 1
	if r.adapter != nil {
		count++
	}
	count += len(r.mitmProxies)
	return count
}

func (r *runtimeServices) shutdown(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, daemonShutdownTimeout)
	defer cancel()
	if r == nil {
		return
	}
	if r.listener != nil {
		_ = r.listener.Close()
	}
	if r.pprofListener != nil {
		_ = r.pprofListener.Close()
	}
	if r.adapter != nil {
		_ = r.adapter.Shutdown(ctx)
	}
	for _, proxy := range r.mitmProxies {
		_ = proxy.Shutdown(ctx)
	}
	// Close the shared capture store after the proxies stop so the SQLite WAL
	// flushes before the process exits.
	if r.captureStore != nil {
		if err := r.captureStore.Close(parent, "shutdown"); err != nil {
			slog.WarnContext(ctx, "daemon.shutdown.capture_store_close_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		}
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
