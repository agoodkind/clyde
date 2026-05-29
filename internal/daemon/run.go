package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/trace"
)

const (
	daemonShutdownTimeout = 5 * time.Second
	reloadDrainCap        = 60 * time.Second
	reloadDrainIdleGrace  = 5 * time.Second

	listenerNameDaemon  = "daemon"
	listenerNameAdapter = "adapter"
	listenerNameMITM    = "mitm"
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

	grpcServer := grpc.NewServer()
	clydev1.RegisterClydeServiceServer(grpcServer, &controlServer{
		UnimplementedClydeServiceServer: clydev1.UnimplementedClydeServiceServer{},
		stats:                           stats,
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
		conversation.DefaultIndex.Start(ctx, time.Minute)
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("daemon.mitm_drift.panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		mitm.RunPeriodicDrift(ctx, cfg.MITM, log)
	}()

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
	listener        net.Listener
	adapter         *adapter.Server
	adapterListener net.Listener
	mitm            *mitm.Proxy
	mitmListener    net.Listener
	errors          chan error
	reloadMu        sync.Mutex
	reloadDrain     <-chan struct{}
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
		listener:        listener,
		adapter:         nil,
		adapterListener: nil,
		mitm:            nil,
		mitmListener:    nil,
		errors:          make(chan error, 3),
		reloadMu:        sync.Mutex{},
		reloadDrain:     nil,
	}
	if cfg.MITM.EnabledDefault {
		proxy, mitmListener, err := startMITM(ctx, cfg, log, runtime.errors, inherited.listeners[listenerNameMITM])
		if err != nil {
			runtime.shutdown(context.WithoutCancel(ctx))
			return nil, err
		}
		runtime.mitm = proxy
		runtime.mitmListener = mitmListener
	} else if inherited.listeners[listenerNameMITM] != nil {
		runtime.shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("mitm listener inherited but mitm is disabled; full daemon restart required")
	}
	if cfg.Adapter.Enabled {
		server, adapterListener, err := startAdapter(ctx, cfg, log, stats, runtime.mitm, runtime.errors, inherited.listeners[listenerNameAdapter])
		if err != nil {
			runtime.shutdown(context.WithoutCancel(ctx))
			return nil, err
		}
		runtime.adapter = server
		runtime.adapterListener = adapterListener
	} else if inherited.listeners[listenerNameAdapter] != nil {
		runtime.shutdown(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("adapter listener inherited but adapter is disabled; full daemon restart required")
	}
	return runtime, nil
}

func startMITM(ctx context.Context, cfg *config.Config, log *slog.Logger, errCh chan<- error, inherited net.Listener) (*mitm.Proxy, net.Listener, error) {
	addr := net.JoinHostPort(normalizeListenHost(cfg.MITM.Listen.Host), strconv.Itoa(cfg.MITM.Listen.Port))
	listener := inherited
	var err error
	if listener != nil {
		if got := listener.Addr().String(); got != addr {
			return nil, nil, fmt.Errorf("mitm inherited listener address %s does not match config %s; full daemon restart required", got, addr)
		}
	} else {
		listenConfig := net.ListenConfig{}
		listener, err = listenConfig.Listen(ctx, "tcp", addr)
		if err != nil {
			log.WarnContext(ctx, "daemon.mitm.listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "addr", addr, "err", err)
			return nil, nil, fmt.Errorf("mitm listen %s: %w", addr, err)
		}
	}
	proxy, err := mitm.NewProxy(cfg.MITM, cfg.Logging.Request, log, listener)
	if err != nil {
		_ = listener.Close()
		log.WarnContext(ctx, "daemon.mitm.init_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "addr", addr, "err", err)
		return nil, nil, fmt.Errorf("init mitm proxy: %w", err)
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.mitm.serve_panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		if serveErr := proxy.Serve(); serveErr != nil {
			errCh <- serveErr
		}
	}()
	log.InfoContext(ctx, "daemon.mitm.started", "concern", "process.daemon.lifecycle", "component", "daemon", "addr", listener.Addr().String())
	return proxy, listener, nil
}

func startAdapter(
	ctx context.Context,
	cfg *config.Config,
	log *slog.Logger,
	stats *providerStatsRecorder,
	proxy *mitm.Proxy,
	errCh chan<- error,
	inherited net.Listener,
) (*adapter.Server, net.Listener, error) {
	deps := adapter.Deps{
		ScratchDir:                   ensureScratchDir,
		RequestEvents:                stats.record,
		RuntimeLogging:               adapter.NewRuntimeLogging(cfg.Logging),
		GetAuth:                      getAuth(cfg, log),
		AnthropicMessagesURLOverride: mitmAnthropicBaseURL(cfg, proxy),
		AnthropicWireBaselinePath:    mitmAnthropicWireBaselinePath(cfg),
	}
	server, err := adapter.New(ctx, cfg.Adapter, cfg.Logging, deps, log)
	if err != nil {
		log.WarnContext(ctx, "daemon.adapter.init_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return nil, nil, fmt.Errorf("adapter init: %w", err)
	}
	listener := inherited
	if listener != nil {
		if got, want := listener.Addr().String(), server.Addr(); got != want {
			return nil, nil, fmt.Errorf("adapter inherited listener address %s does not match config %s; full daemon restart required", got, want)
		}
	} else {
		listenConfig := net.ListenConfig{}
		listener, err = listenConfig.Listen(ctx, "tcp", server.Addr())
		if err != nil {
			log.WarnContext(ctx, "daemon.adapter.listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "addr", server.Addr(), "err", err)
			return nil, nil, fmt.Errorf("adapter listen %s: %w", server.Addr(), err)
		}
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.adapter.serve_panic", "concern", "process.daemon.lifecycle", "component", "daemon", "err", fmt.Sprintf("panic: %v", recovered))
			}
		}()
		if serveErr := server.StartOnListener(ctx, listener); serveErr != nil {
			errCh <- serveErr
		}
	}()
	log.InfoContext(ctx, "daemon.adapter.started", "concern", "process.daemon.lifecycle", "component", "daemon", "addr", listener.Addr().String())
	return server, listener, nil
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

func mitmAnthropicBaseURL(cfg *config.Config, proxy *mitm.Proxy) string {
	if cfg == nil || proxy == nil {
		return ""
	}
	if !cfg.MITM.EnabledDefault || !cfg.MITM.EnabledFor("claude") {
		return ""
	}
	return proxy.BaseURL()
}

// claudeCodeUpstream is the upstream key the MITM drift config and the
// baseline directory use for the claude-cli wire reference.
const claudeCodeUpstream = "claude-code"

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
	if runtime.mitmListener != nil {
		listeners = append(listeners, namedListener{name: listenerNameMITM, lis: runtime.mitmListener})
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

func listenerFile(listener net.Listener) (*os.File, error) {
	switch typed := listener.(type) {
	case *net.UnixListener:
		file, err := typed.File()
		if err != nil {
			slog.Warn("daemon.listener.duplicate_unix_failed", "concern", "process.daemon.lifecycle", "err", err)
			return nil, fmt.Errorf("duplicate unix listener file: %w", err)
		}
		return file, nil
	case *net.TCPListener:
		file, err := typed.File()
		if err != nil {
			slog.Warn("daemon.listener.duplicate_tcp_failed", "concern", "process.daemon.lifecycle", "err", err)
			return nil, fmt.Errorf("duplicate tcp listener file: %w", err)
		}
		return file, nil
	default:
		return nil, fmt.Errorf("unsupported listener type %T", listener)
	}
}

func validateReloadListenerConfig(runtime *runtimeServices) error {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.Warn("daemon.reload.load_config_failed", "concern", "daemon.workers.reload", "err", err)
		return fmt.Errorf("load config for reload validation: %w", err)
	}
	if runtime.adapterListener != nil && !cfg.Adapter.Enabled {
		return fmt.Errorf("adapter listener set changed; full daemon restart required")
	}
	if runtime.adapterListener != nil && runtime.adapter != nil {
		if got, want := runtime.adapterListener.Addr().String(), runtime.adapter.Addr(); got != want {
			return fmt.Errorf("adapter listen address changed from %s to %s; full daemon restart required", got, want)
		}
	}
	if runtime.mitmListener != nil && !cfg.MITM.EnabledDefault {
		return fmt.Errorf("mitm listener set changed; full daemon restart required")
	}
	if runtime.mitmListener != nil {
		want := net.JoinHostPort(normalizeListenHost(cfg.MITM.Listen.Host), strconv.Itoa(cfg.MITM.Listen.Port))
		if got := runtime.mitmListener.Addr().String(); got != want {
			return fmt.Errorf("mitm listen address changed from %s to %s; full daemon restart required", got, want)
		}
	}
	return nil
}

func waitForReplacementDaemon(ctx context.Context, ready io.Reader) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("replacement daemon readiness panic: %v", recovered)
			}
		}()
		data, err := io.ReadAll(ready)
		if err != nil {
			slog.WarnContext(ctx, "daemon.reload.readiness_read_failed", "concern", "daemon.workers.reload", "component", "daemon",
				"err", err,
			)
			done <- fmt.Errorf("read replacement daemon readiness: %w", err)
			return
		}
		if string(data) != "ready\n" {
			done <- fmt.Errorf("replacement daemon readiness failed: %q", string(data))
			return
		}
		done <- nil
	}()
	select {
	case <-deadlineCtx.Done():
		slog.WarnContext(ctx, "daemon.reload.readiness_timeout", "concern", "daemon.workers.reload", "component", "daemon",
			"err", deadlineCtx.Err(),
		)
		return fmt.Errorf("replacement daemon did not become ready: %w", deadlineCtx.Err())
	case err := <-done:
		return err
	}
}

func loadInheritedRuntime() (inheritedRuntime, error) {
	runtime := inheritedRuntime{listeners: make(map[string]net.Listener), ready: nil}
	raw := os.Getenv(daemonsupervisor.EnvInheritedListeners)
	if raw != "" {
		if err := loadInheritedListeners(raw, runtime.listeners); err != nil {
			return runtime, err
		}
	}
	return runtime, nil
}

func loadInheritedListeners(raw string, listeners map[string]net.Listener) error {
	var specs []daemonsupervisor.ListenerSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		slog.Warn("daemon.reload.inherited_specs_decode_failed", "concern", "daemon.workers.reload", "component", "daemon",
			"err", err,
		)
		return fmt.Errorf("decode listener specs: %w", err)
	}
	for _, spec := range specs {
		file := os.NewFile(uintptr(spec.FD), spec.Name)
		if file == nil {
			return fmt.Errorf("listener %s fd %d unavailable", spec.Name, spec.FD)
		}
		listener, err := net.FileListener(file)
		_ = file.Close()
		if err != nil {
			slog.Warn("daemon.reload.inherited_listener_failed", "concern", "daemon.workers.reload", "component", "daemon",
				"name", spec.Name,
				"fd", spec.FD,
				"err", err,
			)
			return fmt.Errorf("listener %s from fd %d: %w", spec.Name, spec.FD, err)
		}
		if listener.Addr().Network() != spec.Network || listener.Addr().String() != spec.Addr {
			_ = listener.Close()
			return fmt.Errorf("listener %s inherited as %s/%s, expected %s/%s", spec.Name, listener.Addr().Network(), listener.Addr().String(), spec.Network, spec.Addr)
		}
		if unixListener, ok := listener.(*net.UnixListener); ok {
			unixListener.SetUnlinkOnClose(false)
		}
		listeners[spec.Name] = listener
	}
	return nil
}

func daemonListener(ctx context.Context, socketPath string, inherited net.Listener) (net.Listener, error) {
	if inherited != nil {
		if unixListener, ok := inherited.(*net.UnixListener); ok {
			unixListener.SetUnlinkOnClose(false)
		}
		slog.InfoContext(ctx, "daemon.listener.inherited", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"addr", inherited.Addr().String(),
		)
		return inherited, nil
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		slog.WarnContext(ctx, "daemon.listener.remove_stale_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("remove stale daemon socket %s: %w", socketPath, err)
	}
	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(ctx, "unix", socketPath)
	if err != nil {
		slog.WarnContext(ctx, "daemon.listener.listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("daemon listen %s: %w", socketPath, err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	slog.InfoContext(ctx, "daemon.listener.started", "concern", "process.daemon.lifecycle", "component", "daemon",
		"socket_path", socketPath,
		"addr", listener.Addr().String(),
	)
	return listener, nil
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
		if r.mitm != nil {
			wg.Go(func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						log.ErrorContext(parent, "daemon.reload.mitm_drain_panic", "concern", "daemon.workers.reload", "component", "daemon",
							"err", fmt.Sprintf("panic: %v", recovered),
						)
					}
				}()
				if err := r.mitm.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace}); err != nil {
					log.WarnContext(ctx, "daemon.reload.mitm_drain_failed", "concern", "daemon.workers.reload", "component", "daemon", "err", err)
				}
			})
		}
		wg.Wait()
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
	if r.mitm != nil {
		count++
	}
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
	if r.adapter != nil {
		_ = r.adapter.Shutdown(ctx)
	}
	if r.mitm != nil {
		_ = r.mitm.Shutdown(ctx)
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
