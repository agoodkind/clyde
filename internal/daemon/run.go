package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/binaryhandoff"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
	codex "goodkind.io/clyde/internal/providers/codex/lifecycle"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/clyde/internal/webapp"
)

var (
	daemonExecutablePath     = os.Executable
	daemonReplacementCommand = exec.Command
	osNewFile                = os.NewFile
	errDaemonAlreadyRunning  = errors.New("daemon already running")
)

// ExtraLoop is an optional background goroutine the daemon owner can
// inject into Run. The loop receives the daemon logger and returns a
// cancel func (or nil if the loop chose not to run). The cancel is
// deferred so the loop shuts down with the daemon. Callers wire
// loops that depend on packages outside the daemon's import graph
// (prune, oauth refresh) without dragging them into the daemon
// package itself.
type ExtraLoop func(log *slog.Logger) func()

const (
	adapterConfigReloadDebounce = 250 * time.Millisecond
	adapterShutdownWait         = 4 * time.Second
)

// reloadHTTPDrainWait caps how long the reload waits for in-flight
// adapter or webapp requests to finish before force-closing. The drain
// polls active-request count rather than sleeping, so idle tunnel
// keepalives return immediately. Active streams get a short grace
// period because the reload RPC is invoked by deploy and must return
// before the CLI deadline.
const (
	reloadHTTPDrainWait         = adapterShutdownWait
	reloadGRPCDrainWait         = 10 * time.Minute
	envDaemonReloadChild        = "CLYDE_DAEMON_RELOAD_CHILD"
	envDaemonInheritedListeners = "CLYDE_DAEMON_INHERITED_LISTENERS"
	envDaemonReadyFD            = "CLYDE_DAEMON_READY_FD"
	envDaemonSupervisorSocket   = "CLYDE_DAEMON_SUPERVISOR_SOCKET"
)

const (
	listenerNameDaemon  = "daemon"
	listenerNameAdapter = "adapter"
	listenerNameWebApp  = "webapp"
	listenerNameMITM    = "mitm"
)

var errReloadBeforeProcessLock = errors.New("daemon reload is unavailable until this daemon owns the process lock")

type replacementDaemonStarter interface {
	startReplacementDaemon(context.Context, *slog.Logger, replacementDaemonRequest) (*replacementDaemonProcess, error)
}

type replacementDaemonRequest struct {
	executablePath string
	files          []*os.File
	specs          []inheritedListenerSpec
	readyWrite     *os.File
	readyFD        int
}

type replacementDaemonProcess struct {
	pid  int
	wait func() error
	kill func() error
}

type directReplacementDaemonStarter struct{}

type adapterLaunchConfig struct {
	Enabled bool
	Adapter config.AdapterConfig
	Logging config.LoggingConfig
}

type adapterProcess struct {
	cancel        context.CancelFunc
	drain         func(context.Context) error
	waitIdle      func(context.Context) int
	activeCount   func() int
	forceClose    func() error
	closeListener func() error
	done          chan struct{}
	lis           net.Listener
}

type adapterController struct {
	log            *slog.Logger
	deps           adapter.Deps
	runtimeLogging *adapter.RuntimeLogging
	mu             sync.Mutex
	current        adapterLaunchConfig
	proc           *adapterProcess
}

type inheritedListenerSpec struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Addr    string `json:"addr"`
	FD      int    `json:"fd"`
}

type inheritedRuntime struct {
	listeners map[string]net.Listener
	ready     *os.File
}

type webAppProcess struct {
	cancel        func()
	drain         func(context.Context) error
	forceClose    func() error
	closeListener func() error
	done          chan struct{}
	lis           net.Listener
	cfg           config.WebAppConfig
}

type mitmProcess struct {
	cancel func()
	drain  func(context.Context) error
	close  func() error
	done   chan struct{}
	lis    net.Listener
	proxy  *mitm.Proxy
}

type daemonRuntime struct {
	listener   net.Listener
	adapter    *adapterController
	webapp     *webAppProcess
	mitm       *mitmProcess
	reloadLock sync.Mutex
}

type exclusiveSubsystems struct {
	log         *slog.Logger
	reloadChild bool
	extraLoops  []ExtraLoop

	mu       sync.Mutex
	cancels  []func()
	stopped  bool
	stopOnce sync.Once
}

type processLockState struct {
	lockHeld     atomic.Bool
	lockAcquired chan struct{}
	release      func(string)
}

func (s *exclusiveSubsystems) addCancel(cancel func()) {
	if cancel == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels = append(s.cancels, cancel)
}

func (s *exclusiveSubsystems) stop(reason string) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		cancels := append([]func(){}, s.cancels...)
		s.cancels = nil
		s.mu.Unlock()
		for _, cancel := range slices.Backward(cancels) {
			cancel()
		}
		s.log.Info("daemon.exclusive_subsystems.stopped",
			"component", "daemon",
			"reason", reason,
		)
	})
}

func (s *exclusiveSubsystems) start() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	for _, loop := range s.extraLoops {
		if loop == nil {
			continue
		}
		if cancel := loop(s.log); cancel != nil {
			s.cancels = append(s.cancels, cancel)
		}
	}
	s.mu.Unlock()
	s.log.Info("daemon.exclusive_subsystems.started",
		"component", "daemon",
		"reload_child", s.reloadChild,
	)
}

func startExclusiveSubsystemsAfterLock(log *slog.Logger, lockAcquired <-chan struct{}, subsystems *exclusiveSubsystems) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warn("daemon.exclusive_subsystems.start_panicked",
					"component", "daemon",
					"panic", r,
				)
			}
		}()
		<-lockAcquired
		subsystems.start()
	}()
}

func acquireDaemonProcessLock(log *slog.Logger, reloadChild bool) (*processLockState, error) {
	lockPath := filepath.Join(config.RuntimeDir(), "daemon.process.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon process lock: %w", err)
	}

	state := &processLockState{
		lockHeld:     atomic.Bool{},
		lockAcquired: make(chan struct{}),
		release:      nil,
	}
	var lockReleaseOnce sync.Once
	state.release = func(reason string) {
		lockReleaseOnce.Do(func() {
			if state.lockHeld.Swap(false) {
				if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
					log.Warn("daemon.process_lock.release_failed",
						"component", "daemon",
						"lock_path", lockPath,
						"reason", reason,
						"err", err,
					)
				} else {
					log.Info("daemon.process_lock.released",
						"component", "daemon",
						"lock_path", lockPath,
						"reason", reason,
					)
				}
			}
			_ = lockFile.Close()
		})
	}

	if reloadChild {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warn("daemon.reload_child.lock_goroutine_panicked",
						"component", "daemon",
						"lock_path", lockPath,
						"panic", r,
					)
				}
			}()
			acquireReloadChildProcessLock(log, lockFile, lockPath, &state.lockHeld, state.lockAcquired)
		}()
		return state, nil
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		log.Info("daemon.already_running",
			"component", "daemon",
			"lock_path", lockPath)
		return nil, errDaemonAlreadyRunning
	}
	state.lockHeld.Store(true)
	close(state.lockAcquired)
	return state, nil
}

// Run starts the daemon gRPC server on the XDG runtime Unix socket
// and, when the user enables it, the OpenAI compatible HTTP adapter
// on a local port. A single launchd entry boots both layers so the
// monolith stays one process. Additional opt-in background loops
// (prune, oauth refresh) are passed in by the caller.
func Run(log *slog.Logger, extraLoops ...ExtraLoop) error {
	log = slogger.WithConcern(log, slogger.ConcernProcessDaemonLifecycle)
	if err := config.EnsureRuntimeDir(); err != nil {
		return err
	}

	reloadChild := os.Getenv(envDaemonReloadChild) == "1"
	processLock, err := acquireDaemonProcessLock(log, reloadChild)
	if err != nil {
		if errors.Is(err, errDaemonAlreadyRunning) {
			return nil
		}
		return err
	}
	defer processLock.release("exit")

	socketPath := config.DaemonSocketPath()

	inherited, err := loadInheritedRuntime()
	if err != nil {
		return fmt.Errorf("load inherited listeners: %w", err)
	}
	listener, err := daemonListener(socketPath, inherited.listeners[listenerNameDaemon])
	if err != nil {
		return err
	}

	srv, err := New(log)
	if err != nil {
		return fmt.Errorf("failed to create daemon server: %w", err)
	}
	defer func() { srv.Close() }()

	// Wire the auto-rename worker package through a no-op probe so the
	// production binary keeps internal/sessionrename reachable until
	// the follow-on PR ships the daemon scheduler that drives it.
	if cfg, cfgErr := config.LoadGlobalOrDefault(); cfgErr == nil {
		autoNameProbeReachability(context.Background(), cfg, log)
	}

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(daemonUnaryCorrelationInterceptor(log)),
		grpc.StreamInterceptor(daemonStreamCorrelationInterceptor(log)),
	)
	clydev1.RegisterClydeServiceServer(grpcServer, srv)

	subsystems, err := startDaemonSubsystems(log, srv, inherited)
	if err != nil {
		return err
	}
	rt := &daemonRuntime{
		listener:   listener,
		adapter:    subsystems.adapterCtrl,
		webapp:     subsystems.webProc,
		mitm:       subsystems.mitmProc,
		reloadLock: sync.Mutex{},
	}

	exclusive := configureExclusiveSubsystems(log, reloadChild, extraLoops, subsystems.adapterCancel, subsystems.webProc, subsystems.mitmProc, processLock.lockAcquired)
	defer exclusive.stop("exit")

	setReloadFuncWhenProcessOwner(srv, &processLock.lockHeld, func(ctx context.Context) (reloadReport, error) {
		return reloadDaemonBinary(ctx, log, grpcServer, rt, srv, exclusive.stop, processLock.release)
	})

	log.Info("daemon.listening",
		"component", "daemon",
		"socket", socketPath,
	)
	if inherited.ready != nil {
		errCh := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warn("daemon.grpc.serve_panicked",
						"component", "daemon",
						"panic", r,
					)
					errCh <- fmt.Errorf("grpc serve panic: %v", r)
				}
			}()
			errCh <- grpcServer.Serve(listener)
		}()
		_, _ = inherited.ready.WriteString("ready\n")
		_ = inherited.ready.Close()
		return <-errCh
	}
	return grpcServer.Serve(listener)
}

type daemonSubsystems struct {
	mitmProc      *mitmProcess
	adapterCtrl   *adapterController
	adapterCancel func()
	webProc       *webAppProcess
}

// startDaemonSubsystems boots the MITM proxy, adapter, and webapp in
// dependency order so the adapter sees the proxy URL and the webapp
// can rely on srv being wired. Cancellations roll back partial
// startup if any later subsystem fails.
func startDaemonSubsystems(log *slog.Logger, srv *Server, inherited inheritedRuntime) (daemonSubsystems, error) {
	mitmProc, err := startMITM(log, inherited.listeners[listenerNameMITM])
	if err != nil {
		log.Error("daemon.subsystems.mitm_failed", "component", "daemon", "err", err)
		return daemonSubsystems{}, fmt.Errorf("mitm startup: %w", err)
	}
	srv.SetMITMProxyAccessor(func() *mitm.Proxy {
		if mitmProc == nil {
			return nil
		}
		return mitmProc.proxy
	})
	adapterCtrl, adapterCancel, err := startAdapter(log, srv, inherited.listeners[listenerNameAdapter], mitmProc)
	if err != nil {
		log.Error("daemon.subsystems.adapter_failed", "component", "daemon", "err", err)
		if mitmProc != nil && mitmProc.cancel != nil {
			mitmProc.cancel()
		}
		return daemonSubsystems{}, fmt.Errorf("adapter startup: %w", err)
	}
	webProc, err := startWebApp(log, srv, inherited.listeners[listenerNameWebApp])
	if err != nil {
		log.Error("daemon.subsystems.webapp_failed", "component", "daemon", "err", err)
		if adapterCancel != nil {
			adapterCancel()
		}
		if mitmProc != nil && mitmProc.cancel != nil {
			mitmProc.cancel()
		}
		return daemonSubsystems{}, fmt.Errorf("webapp startup: %w", err)
	}
	return daemonSubsystems{
		mitmProc:      mitmProc,
		adapterCtrl:   adapterCtrl,
		adapterCancel: adapterCancel,
		webProc:       webProc,
	}, nil
}

func configureExclusiveSubsystems(log *slog.Logger, reloadChild bool, extraLoops []ExtraLoop, adapterCancel func(), webProc *webAppProcess, mitmProc *mitmProcess, lockAcquired <-chan struct{}) *exclusiveSubsystems {
	exclusive := &exclusiveSubsystems{
		log:         log,
		reloadChild: reloadChild,
		extraLoops:  extraLoops,
		mu:          sync.Mutex{},
		cancels:     nil,
		stopped:     false,
		stopOnce:    sync.Once{},
	}
	exclusive.addCancel(adapterCancel)
	if webProc != nil && webProc.cancel != nil {
		exclusive.addCancel(webProc.cancel)
	}
	if mitmProc != nil && mitmProc.cancel != nil {
		exclusive.addCancel(mitmProc.cancel)
	}
	if reloadChild {
		startExclusiveSubsystemsAfterLock(log, lockAcquired, exclusive)
	} else {
		exclusive.start()
	}
	return exclusive
}

func setReloadFuncWhenProcessOwner(srv *Server, lockHeld *atomic.Bool, fn func(context.Context) (reloadReport, error)) {
	srv.SetReloadFunc(func(ctx context.Context) (reloadReport, error) {
		if lockHeld == nil || !lockHeld.Load() {
			return reloadReport{}, errReloadBeforeProcessLock
		}
		return fn(ctx)
	})
}

func acquireReloadChildProcessLock(log *slog.Logger, lockFile *os.File, lockPath string, lockHeld *atomic.Bool, lockAcquired chan<- struct{}) {
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		log.Warn("daemon.reload_child.lock_failed",
			"component", "daemon",
			"lock_path", lockPath,
			"err", err,
		)
		return
	}
	lockHeld.Store(true)
	close(lockAcquired)
	log.Info("daemon.reload_child.lock_acquired",
		"component", "daemon",
		"lock_path", lockPath,
	)
}

func daemonListener(socketPath string, inherited net.Listener) (net.Listener, error) {
	if inherited != nil {
		if unixListener, ok := inherited.(*net.UnixListener); ok {
			unixListener.SetUnlinkOnClose(false)
		}
		return inherited, nil
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		slog.WarnContext(context.Background(), "daemon.listener.remove_stale_failed",
			"component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("failed to remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		slog.WarnContext(context.Background(), "daemon.listener.listen_failed",
			"component", "daemon",
			"socket_path", socketPath,
			"err", err,
		)
		return nil, fmt.Errorf("failed to listen on %s: %w", socketPath, err)
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	return listener, nil
}

func loadInheritedRuntime() (inheritedRuntime, error) {
	out := inheritedRuntime{listeners: make(map[string]net.Listener)}
	raw := os.Getenv(envDaemonInheritedListeners)
	if raw != "" {
		var err error
		out, err = loadInheritedListeners(raw, out)
		if err != nil {
			return out, err
		}
	}
	if rawFD := os.Getenv(envDaemonReadyFD); rawFD != "" {
		fd, err := strconv.Atoi(rawFD)
		if err != nil {
			slog.WarnContext(context.Background(), "daemon.reload.ready_fd_parse_failed",
				"component", "daemon",
				"ready_fd", rawFD,
				"err", err,
			)
			return out, fmt.Errorf("parse ready fd: %w", err)
		}
		out.ready = osNewFile(uintptr(fd), "daemon-ready")
		if out.ready == nil {
			return out, fmt.Errorf("ready fd %d unavailable", fd)
		}
	}
	return out, nil
}

func loadInheritedListeners(raw string, out inheritedRuntime) (inheritedRuntime, error) {
	var specs []inheritedListenerSpec
	if err := json.Unmarshal([]byte(raw), &specs); err != nil {
		slog.WarnContext(context.Background(), "daemon.reload.inherited_specs_decode_failed",
			"component", "daemon",
			"err", err,
		)
		return out, fmt.Errorf("decode listener specs: %w", err)
	}
	for _, spec := range specs {
		file := osNewFile(uintptr(spec.FD), spec.Name)
		if file == nil {
			return out, fmt.Errorf("listener %s fd %d unavailable", spec.Name, spec.FD)
		}
		lis, err := net.FileListener(file)
		_ = file.Close()
		if err != nil {
			slog.WarnContext(context.Background(), "daemon.reload.inherited_listener_failed",
				"component", "daemon",
				"name", spec.Name,
				"fd", spec.FD,
				"err", err,
			)
			return out, fmt.Errorf("listener %s from fd %d: %w", spec.Name, spec.FD, err)
		}
		if lis.Addr().Network() != spec.Network || lis.Addr().String() != spec.Addr {
			_ = lis.Close()
			return out, fmt.Errorf("listener %s inherited as %s/%s, expected %s/%s", spec.Name, lis.Addr().Network(), lis.Addr().String(), spec.Network, spec.Addr)
		}
		if unixListener, ok := lis.(*net.UnixListener); ok {
			unixListener.SetUnlinkOnClose(false)
		}
		out.listeners[spec.Name] = lis
	}
	return out, nil
}

func reloadDaemonBinary(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, rt *daemonRuntime, srv *Server, stopExclusive func(string), releaseProcessLock func(string)) (reloadReport, error) {
	rt.reloadLock.Lock()
	defer rt.reloadLock.Unlock()
	executablePath, err := validatedReplacementDaemonPath(ctx, log)
	if err != nil {
		return reloadReport{}, err
	}

	files, specs, cleanup, err := inheritedListenerFiles(rt)
	if err != nil {
		return reloadReport{}, err
	}
	defer cleanup()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		log.WarnContext(ctx, "daemon.reload.ready_pipe_failed", "component", "daemon", "err", err)
		return reloadReport{}, fmt.Errorf("create reload readiness pipe: %w", err)
	}
	defer func() { _ = readyRead.Close() }()
	defer func() { _ = readyWrite.Close() }()
	readyFD := 3 + len(files)

	starter := replacementDaemonStarterForCurrentPlatform()
	proc, err := starter.startReplacementDaemon(ctx, log, replacementDaemonRequest{
		executablePath: executablePath,
		files:          files,
		specs:          specs,
		readyWrite:     readyWrite,
		readyFD:        readyFD,
	})
	if err != nil {
		return reloadReport{}, err
	}
	_ = readyWrite.Close()
	watchReplacementDaemon(ctx, log, proc)

	if err := waitForReplacementDaemon(ctx, readyRead); err != nil {
		_ = proc.kill()
		return reloadReport{}, err
	}
	srv.preserveRuntimeDirsOnClose()
	drainReloadedPublicHTTP(log, rt)
	if stopExclusive != nil {
		stopExclusive("reload_handoff")
	}
	grpcDrainStarted := startReloadGRPCDrain(ctx, log, grpcServer, proc)
	<-grpcDrainStarted
	if releaseProcessLock != nil {
		releaseProcessLock("reload_handoff")
	}
	return reloadReport{BinaryReloaded: true, NewPID: proc.pid}, nil
}

func watchReplacementDaemon(ctx context.Context, log *slog.Logger, proc *replacementDaemonProcess) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "daemon.reload.replacement_wait_panicked",
					"component", "daemon",
					"new_pid", proc.pid,
					"panic", r,
				)
			}
		}()
		_ = proc.wait()
	}()
}

func startReloadGRPCDrain(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, proc *replacementDaemonProcess) <-chan struct{} {
	grpcDrainStarted := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "daemon.reload.grpc_drain_panicked",
					"component", "daemon",
					"new_pid", proc.pid,
					"panic", r,
				)
			}
		}()
		log.InfoContext(ctx, "daemon.reload.draining_old_process",
			"component", "daemon",
			"new_pid", proc.pid,
			"timeout", reloadGRPCDrainWait.String(),
		)
		done := startGracefulGRPCStop(ctx, log, grpcServer, proc, grpcDrainStarted)
		select {
		case <-done:
			log.InfoContext(ctx, "daemon.reload.old_process_grpc_drain_complete",
				"component", "daemon",
				"new_pid", proc.pid,
			)
		case <-time.After(reloadGRPCDrainWait):
			log.WarnContext(ctx, "daemon.reload.old_process_grpc_drain_timeout",
				"component", "daemon",
				"new_pid", proc.pid,
				"timeout", reloadGRPCDrainWait.String(),
			)
			grpcServer.Stop()
			<-done
		}
	}()
	return grpcDrainStarted
}

func startGracefulGRPCStop(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, proc *replacementDaemonProcess, grpcDrainStarted chan<- struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "daemon.reload.grpc_graceful_stop_panicked",
					"component", "daemon",
					"new_pid", proc.pid,
					"panic", r,
				)
			}
		}()
		close(grpcDrainStarted)
		grpcServer.GracefulStop()
	}()
	return done
}

func validatedReplacementDaemonPath(ctx context.Context, log *slog.Logger) (string, error) {
	executablePath, err := daemonExecutablePath()
	if err != nil {
		return "", fmt.Errorf("resolve executable: %w", err)
	}
	executablePath, err = filepath.Abs(executablePath)
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if err := binaryhandoff.ValidateClydeExecutable(executablePath); err != nil {
		log.WarnContext(ctx, "daemon.reload.replacement_rejected",
			"component", "daemon",
			"path", executablePath,
			"err", err)
		return "", fmt.Errorf("validate replacement daemon binary: %w", err)
	}
	return executablePath, nil
}

func replacementDaemonStarterForCurrentPlatform() replacementDaemonStarter {
	return replacementDaemonStarterForPlatform(runtime.GOOS, os.Getenv(envDaemonSupervisorSocket))
}

func replacementDaemonStarterForPlatform(goos string, supervisorSocket string) replacementDaemonStarter {
	if goos == "darwin" {
		socketPath := strings.TrimSpace(supervisorSocket)
		if socketPath == "" {
			socketPath = supervisorSocketPath()
		}
		return supervisorReplacementDaemonStarter{socketPath: socketPath}
	}
	return directReplacementDaemonStarter{}
}

func (directReplacementDaemonStarter) startReplacementDaemon(_ context.Context, log *slog.Logger, req replacementDaemonRequest) (*replacementDaemonProcess, error) {
	cmd := daemonReplacementCommand(req.executablePath, "daemon")
	cmd.Stdout = nil
	cmd.Stderr = nil
	extraFiles := append([]*os.File{}, req.files...)
	extraFiles = append(extraFiles, req.readyWrite)
	cmd.ExtraFiles = extraFiles
	specJSON, err := json.Marshal(req.specs)
	if err != nil {
		log.Warn("daemon.reload.inherited_listeners_encode_failed",
			"component", "daemon",
			"path", req.executablePath,
			"err", err)
		return nil, fmt.Errorf("encode inherited listeners: %w", err)
	}
	cmd.Env = daemonEnvWithOverrides(os.Environ(),
		envDaemonReloadChild+"=1",
		envDaemonInheritedListeners+"="+string(specJSON),
		envDaemonReadyFD+"="+strconv.Itoa(req.readyFD),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Warn("daemon.reload.replacement_start_failed",
			"component", "daemon",
			"path", req.executablePath,
			"err", err)
		return nil, fmt.Errorf("start replacement daemon: %w", err)
	}
	return &replacementDaemonProcess{
		pid:  cmd.Process.Pid,
		wait: cmd.Wait,
		kill: cmd.Process.Kill,
	}, nil
}

func drainReloadedPublicHTTP(log *slog.Logger, rt *daemonRuntime) {
	if rt == nil {
		return
	}
	if rt.adapter != nil {
		rt.adapter.drainReloadedProcess(reloadHTTPDrainWait)
	}
	drainReloadedMITM(log, rt)
	if rt.webapp != nil && rt.webapp.drain != nil {
		log.Info("daemon.reload.draining_old_webapp",
			"component", "daemon",
			"addr", listenerAddr(rt.webapp.lis),
		)
		if rt.webapp.closeListener != nil {
			if err := rt.webapp.closeListener(); err != nil && !errors.Is(err, net.ErrClosed) {
				log.Warn("daemon.reload.webapp_listener_close_failed",
					"component", "daemon",
					"addr", listenerAddr(rt.webapp.lis),
					"err", err,
				)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), reloadHTTPDrainWait)
		err := rt.webapp.drain(ctx)
		cancel()
		if err != nil {
			log.Warn("daemon.reload.webapp_drain_timeout",
				"component", "daemon",
				"addr", listenerAddr(rt.webapp.lis),
				"err", err,
			)
		} else {
			log.Info("daemon.reload.webapp_drain_complete",
				"component", "daemon",
				"addr", listenerAddr(rt.webapp.lis),
			)
		}
		if rt.webapp.forceClose != nil {
			if err := rt.webapp.forceClose(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				log.Warn("daemon.reload.webapp_force_close_failed",
					"component", "daemon",
					"addr", listenerAddr(rt.webapp.lis),
					"err", err,
				)
			} else if err != nil {
				log.Debug("daemon.reload.webapp_force_closed",
					"component", "daemon",
					"addr", listenerAddr(rt.webapp.lis),
				)
			}
		}
	}
}

func drainReloadedMITM(log *slog.Logger, rt *daemonRuntime) {
	if rt == nil || rt.mitm == nil {
		return
	}
	mitmProc := rt.mitm
	addr := listenerAddr(mitmProc.lis)
	log.Info("daemon.reload.draining_old_mitm",
		"component", "daemon",
		"addr", addr,
	)
	if mitmProc.close != nil {
		if err := mitmProc.close(); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Warn("daemon.reload.mitm_listener_close_failed",
				"component", "daemon",
				"addr", addr,
				"err", err,
			)
		}
	}
	if mitmProc.drain == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), reloadHTTPDrainWait)
	err := mitmProc.drain(ctx)
	cancel()
	if err != nil {
		log.Warn("daemon.reload.mitm_drain_timeout",
			"component", "daemon",
			"addr", addr,
			"err", err,
		)
		return
	}
	log.Info("daemon.reload.mitm_drain_complete",
		"component", "daemon",
		"addr", addr,
	)
}

func waitForReplacementDaemon(ctx context.Context, ready io.Reader) error {
	deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.WarnContext(ctx, "daemon.reload.readiness_wait_panicked",
					"component", "daemon",
					"panic", r,
				)
				done <- fmt.Errorf("replacement daemon readiness panic: %v", r)
			}
		}()
		data, err := io.ReadAll(ready)
		if err != nil {
			done <- err
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
		slog.WarnContext(ctx, "daemon.reload.readiness_timeout",
			"component", "daemon",
			"err", deadlineCtx.Err(),
		)
		return fmt.Errorf("replacement daemon did not become ready: %w", deadlineCtx.Err())
	case err := <-done:
		return err
	}
}

func inheritedListenerFiles(rt *daemonRuntime) ([]*os.File, []inheritedListenerSpec, func(), error) {
	if rt == nil || rt.listener == nil {
		return nil, nil, func() {}, fmt.Errorf("daemon listener is not available for reload")
	}
	if err := validateReloadListenerConfig(rt); err != nil {
		return nil, nil, func() {}, err
	}
	type namedListener struct {
		name string
		lis  net.Listener
	}
	listeners := []namedListener{{name: listenerNameDaemon, lis: rt.listener}}
	if rt.adapter != nil {
		if lis := rt.adapter.listener(); lis != nil {
			listeners = append(listeners, namedListener{name: listenerNameAdapter, lis: lis})
		}
	}
	if rt.webapp != nil && rt.webapp.lis != nil {
		listeners = append(listeners, namedListener{name: listenerNameWebApp, lis: rt.webapp.lis})
	}
	if rt.mitm != nil && rt.mitm.lis != nil {
		listeners = append(listeners, namedListener{name: listenerNameMITM, lis: rt.mitm.lis})
	}
	files := make([]*os.File, 0, len(listeners))
	specs := make([]inheritedListenerSpec, 0, len(listeners))
	cleanup := func() {
		for _, f := range files {
			_ = f.Close()
		}
	}
	for _, named := range listeners {
		file, err := listenerFile(named.lis)
		if err != nil {
			cleanup()
			slog.WarnContext(context.Background(), "daemon.reload.inherit_listener_failed",
				"component", "daemon",
				"name", named.name,
				"err", err,
			)
			return nil, nil, func() {}, fmt.Errorf("inherit listener %s: %w", named.name, err)
		}
		files = append(files, file)
		specs = append(specs, inheritedListenerSpec{
			Name:    named.name,
			Network: named.lis.Addr().Network(),
			Addr:    named.lis.Addr().String(),
			FD:      3 + len(files) - 1,
		})
	}
	return files, specs, cleanup, nil
}

func listenerFile(lis net.Listener) (*os.File, error) {
	switch l := lis.(type) {
	case *net.UnixListener:
		return l.File()
	case *net.TCPListener:
		return l.File()
	default:
		return nil, fmt.Errorf("unsupported listener type %T", lis)
	}
}

func listenerAddr(lis net.Listener) string {
	if lis == nil {
		return ""
	}
	return lis.Addr().String()
}

func validateReloadListenerConfig(rt *daemonRuntime) error {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return fmt.Errorf("load config for reload validation: %w", err)
	}
	adapterRunning := rt.adapter != nil && rt.adapter.listener() != nil
	if adapterRunning != cfg.Adapter.Enabled {
		return fmt.Errorf("adapter listener set changed; full daemon restart required")
	}
	if adapterRunning {
		if got, want := rt.adapter.listener().Addr().String(), adapterListenAddr(cfg.Adapter); got != want {
			return fmt.Errorf("adapter listen address changed from %s to %s; full daemon restart required", got, want)
		}
	}
	webRunning := rt.webapp != nil && rt.webapp.lis != nil
	if webRunning != cfg.WebApp.Enabled {
		return fmt.Errorf("webapp listener set changed; full daemon restart required")
	}
	if webRunning {
		if got, want := rt.webapp.lis.Addr().String(), webAppListenAddr(cfg.WebApp); got != want {
			return fmt.Errorf("webapp listen address changed from %s to %s; full daemon restart required", got, want)
		}
	}
	mitmRunning := rt.mitm != nil && rt.mitm.lis != nil
	if mitmRunning {
		if got, want := rt.mitm.lis.Addr().String(), mitmListenAddr(cfg.MITM); got != want {
			return fmt.Errorf("mitm listen address changed from %s to %s; full daemon restart required", got, want)
		}
	}
	return nil
}

func adapterListenAddr(cfg config.AdapterConfig) string {
	host := cfg.Host
	if host == "" {
		host = adapter.DefaultHost
	}
	port := cfg.Port
	if port <= 0 {
		port = adapter.DefaultPort
	}
	return net.JoinHostPort(normalizeListenHost(host), strconv.Itoa(port))
}

// mitmListenAddr returns the host:port the daemon-owned MITM proxy
// binds. Defaults match config-side defaults so callers can compute
// the address before config is applied. Defensive against zero
// values even though config.Load applies the same defaults upstream.
func mitmListenAddr(cfg config.MITMConfig) string {
	host := cfg.Listen.Host
	if strings.TrimSpace(host) == "" {
		host = "[::1]"
	}
	port := cfg.Listen.Port
	if port <= 0 {
		port = 48723
	}
	return net.JoinHostPort(normalizeListenHost(host), strconv.Itoa(port))
}

func webAppListenAddr(cfg config.WebAppConfig) string {
	host := cfg.Host
	if host == "" {
		host = webapp.DefaultHost
	}
	port := cfg.Port
	if port <= 0 {
		port = webapp.DefaultPort
	}
	return net.JoinHostPort(normalizeListenHost(host), strconv.Itoa(port))
}

func normalizeListenHost(host string) string {
	trimmed := strings.TrimSpace(host)
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		if strings.Contains(inner, ":") {
			return inner
		}
	}
	return trimmed
}

// startWebApp boots the optional remote dashboard. The webapp shares
// the daemon process so a single launchd entry covers gRPC, the
// OpenAI adapter, and the dashboard.
func startWebApp(log *slog.Logger, srv *Server, inherited net.Listener) (*webAppProcess, error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.Warn("webapp.config_load_failed",
			"component", "webapp",
			"err", err,
		)
		return nil, nil
	}
	if !cfg.WebApp.Enabled {
		if inherited != nil {
			return nil, fmt.Errorf("webapp listener inherited but webapp is disabled; full daemon restart required")
		}
		return nil, nil
	}
	deps := webapp.Deps{
		ListLiveSessions:  srv.listLiveSessionsForWebApp,
		StartLiveSession:  srv.startLiveSessionForWebApp,
		SendLiveSession:   srv.sendLiveSessionForWebApp,
		StreamLiveSession: srv.streamLiveSessionForWebApp,
		StopLiveSession:   srv.stopLiveSessionForWebApp,
	}
	srvW := webapp.New(cfg.WebApp, deps, log)
	lis := inherited
	if lis != nil {
		if got, want := lis.Addr().String(), srvW.Addr(); got != want {
			return nil, fmt.Errorf("webapp inherited listener address %s does not match config %s; full daemon restart required", got, want)
		}
	} else {
		lis, err = net.Listen("tcp", srvW.Addr())
		if err != nil {
			return nil, fmt.Errorf("webapp listen %s: %w", srvW.Addr(), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warn("webapp.run_panicked",
					"component", "webapp",
					"panic", r,
				)
			}
		}()
		defer close(done)
		if err := srvW.StartOnListener(ctx, lis); err != nil {
			log.Error("webapp.exited",
				"component", "webapp",
				"err", err,
			)
		}
	}()
	return &webAppProcess{
		cancel:        cancel,
		drain:         srvW.Shutdown,
		forceClose:    srvW.Close,
		closeListener: lis.Close,
		done:          done,
		lis:           lis,
		cfg:           cfg.WebApp,
	}, nil
}

// startMITM boots the daemon-owned MITM capture proxy on the
// configured stable listen address. The proxy serves callers like the
// adapter (Claude routing), Claude CLI baseline capture, and drift
// refresh until daemon shutdown. The listener is config-pinned so
// reload preserves the bind without dropping in-flight tunnels; the
// daemon inherits the listener FD across re-exec.
func startMITM(log *slog.Logger, inherited net.Listener) (*mitmProcess, error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.Warn("mitm.config_load_failed",
			"component", "mitm",
			"err", err,
		)
		return nil, fmt.Errorf("load config for mitm: %w", err)
	}
	wantAddr := mitmListenAddr(cfg.MITM)
	lis := inherited
	if lis != nil {
		if got := lis.Addr().String(); got != wantAddr {
			return nil, fmt.Errorf("mitm inherited listener address %s does not match config %s; full daemon restart required", got, wantAddr)
		}
	} else {
		lis, err = net.Listen("tcp", wantAddr)
		if err != nil {
			return nil, fmt.Errorf("mitm listen %s: %w", wantAddr, err)
		}
	}
	proxy, err := mitm.NewProxy(cfg.MITM, log, lis)
	if err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("mitm proxy: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warn("mitm.run_panicked",
					"component", "mitm",
					"panic", r,
				)
			}
		}()
		defer close(done)
		if err := proxy.Serve(); err != nil {
			log.Error("mitm.exited",
				"component", "mitm",
				"err", err,
			)
		}
	}()
	cancel := func() {
		ctx, cancelTimeout := context.WithTimeout(context.Background(), adapterShutdownWait)
		defer cancelTimeout()
		if err := proxy.Shutdown(ctx); err != nil {
			log.Warn("mitm.shutdown_failed",
				"component", "mitm",
				"err", err,
			)
		}
	}
	return &mitmProcess{
		cancel: cancel,
		drain:  proxy.Shutdown,
		close:  lis.Close,
		done:   done,
		lis:    lis,
		proxy:  proxy,
	}, nil
}

func (s *Server) listLiveSessionsForWebApp(context.Context) ([]webapp.LiveSession, error) {
	resp, err := s.ListLiveSessions(context.Background(), &clydev1.ListLiveSessionsRequest{})
	if err != nil {
		return nil, err
	}
	out := make([]webapp.LiveSession, 0, len(resp.GetSessions()))
	for _, live := range resp.GetSessions() {
		out = append(out, webLiveSessionFromProto(live))
	}
	return out, nil
}

func (s *Server) startLiveSessionForWebApp(ctx context.Context, req webapp.StartLiveSessionRequest) (webapp.LiveSession, error) {
	resp, err := s.StartLiveSession(ctx, &clydev1.StartLiveSessionRequest{
		Provider:  req.Provider,
		Name:      req.Name,
		Basedir:   req.Basedir,
		Model:     req.Model,
		Effort:    req.Effort,
		Incognito: req.Incognito,
	})
	if err != nil {
		return webapp.LiveSession{}, err
	}
	return webLiveSessionFromProto(resp.GetSession()), nil
}

func (s *Server) sendLiveSessionForWebApp(ctx context.Context, sessionID, text string) error {
	resp, err := s.SendLiveSession(ctx, &clydev1.SendLiveSessionRequest{
		SessionId: sessionID,
		Text:      text,
	})
	if err != nil {
		return err
	}
	if !resp.GetAccepted() {
		return fmt.Errorf("live session %q did not accept input", sessionID)
	}
	return nil
}

func (s *Server) streamLiveSessionForWebApp(ctx context.Context, sessionID string) (<-chan webapp.LiveSessionEvent, error) {
	_, _ = peer.FromContext(ctx)
	return s.streamLiveSessionEvents(ctx, sessionID)
}

func (s *Server) stopLiveSessionForWebApp(ctx context.Context, sessionID string) error {
	_, err := s.StopLiveSession(ctx, &clydev1.StopLiveSessionRequest{SessionId: sessionID})
	return err
}

func webLiveSessionFromProto(live *clydev1.LiveSession) webapp.LiveSession {
	if live == nil {
		return webapp.LiveSession{}
	}
	startedAt := time.Time{}
	if live.GetStartedAtNanos() > 0 {
		startedAt = time.Unix(0, live.GetStartedAtNanos())
	}
	return webapp.LiveSession{
		Provider:       live.GetProvider(),
		SessionName:    live.GetSessionName(),
		SessionID:      live.GetSessionId(),
		Status:         live.GetStatus(),
		Basedir:        live.GetBasedir(),
		URL:            live.GetUrl(),
		StartedAt:      startedAt,
		SupportsSend:   live.GetSupportsSend(),
		SupportsStream: live.GetSupportsStream(),
		SupportsStop:   live.GetSupportsStop(),
	}
}

func (s *Server) liveSessionRecord(ctx context.Context, sessionID string) (*liveRuntimeSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("session id is required")
	}
	s.remoteMu.Lock()
	live := s.liveSessions[sessionID]
	if live != nil {
		s.remoteMu.Unlock()
		return live, nil
	}
	runtime := newCodexLiveRuntime(codex.LiveRuntimeOptions{})
	attached, err := runtime.Attach(ctx, codex.LiveAttachRequest{ThreadID: sessionID})
	if err != nil {
		s.remoteMu.Unlock()
		_ = runtime.Close()
		return nil, err
	}
	live = &liveRuntimeSession{
		provider:     session.ProviderCodex,
		name:         attached.ThreadID,
		id:           attached.ThreadID,
		basedir:      attached.WorkDir,
		model:        attached.Model,
		status:       "attached",
		startedAt:    daemonNow(),
		codexRuntime: runtime,
	}
	s.liveSessions[live.id] = live
	s.remoteMu.Unlock()
	return live, nil
}

// startAdapter reads the global config and launches the HTTP server
// when enabled. A cancel func is returned so Run can shut the
// listener down on exit.
//
// Returns an error when the adapter is enabled but
// adapter.New rejects the config (missing families, default model,
// or required client_identity fields). The daemon then exits non-zero so
// launchd reports the failure instead of silently running without
// the OpenAI surface the user asked for.
func startAdapter(log *slog.Logger, srv *Server, inherited net.Listener, mitmProc *mitmProcess) (*adapterController, func(), error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.Warn("adapter.config_load_failed",
			"component", "adapter",
			"err", err,
		)
		return nil, nil, nil
	}

	mitmOverride := adapterMITMOverride(*cfg, log, mitmProc)

	ctrl := &adapterController{
		log:            log,
		runtimeLogging: adapter.NewRuntimeLogging(cfg.Logging),
		deps: adapter.Deps{
			ResolveClaude:                findRealClaude,
			ScratchDir:                   adapterScratchDir,
			RequestEvents:                srv.providerStats.Record,
			AnthropicMessagesURLOverride: mitmOverride,
		},
	}
	if err := ctrl.apply(context.Background(), launchConfigFromGlobal(cfg), true, inherited); err != nil {
		return nil, nil, err
	}

	stopWatch, err := watchAdapterConfig(log, ctrl)
	if err != nil {
		log.Warn("adapter.config_watch_failed",
			"component", "adapter",
			"err", err,
		)
	}
	return ctrl, func() {
		if stopWatch != nil {
			stopWatch()
		}
		ctrl.stop()
	}, nil
}

func adapterMITMOverride(cfg config.Config, log *slog.Logger, mitmProc *mitmProcess) string {
	// When [mitm].enabled_default is set and the provider list
	// includes "claude", route the adapter's outbound /v1/messages
	// through the daemon-owned MITM proxy. This lets us capture our
	// own outbound and diff against the claude-cli reference snapshot
	// (CLYDE-124 live verification). The proxy is daemon-owned and
	// already running by the time the adapter starts.
	if !cfg.MITM.EnabledDefault || !cfg.MITM.EnabledFor("claude") {
		return ""
	}
	if mitmProc == nil || mitmProc.proxy == nil {
		log.Warn("adapter.mitm.proxy_unavailable",
			"component", "adapter",
		)
		return ""
	}
	mitmOverride := mitmProc.proxy.ClaudeBaseURL()
	log.Info("adapter.mitm.routing_enabled",
		"component", "adapter",
		"proxy_base", mitmOverride,
	)
	return mitmOverride
}

func launchConfigFromGlobal(cfg *config.Config) adapterLaunchConfig {
	if cfg == nil {
		return adapterLaunchConfig{}
	}
	return adapterLaunchConfig{
		Enabled: cfg.Adapter.Enabled,
		Adapter: cfg.Adapter,
		Logging: cfg.Logging,
	}
}

func watchAdapterConfig(log *slog.Logger, ctrl *adapterController) (func(), error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	configDir := filepath.Dir(config.GlobalConfigPath())
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	if err := watcher.Add(configDir); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("watch config dir: %w", err)
	}
	tomlPath := filepath.Clean(filepath.Join(configDir, "config.toml"))
	log.Info("adapter.config_watch.started",
		"component", "adapter",
		"dir", configDir,
		"toml_path", tomlPath,
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warn("adapter.config_watch.panicked",
					"component", "adapter",
					"panic", r,
				)
			}
		}()
		defer close(done)
		var timer *time.Timer
		var timerCh <-chan time.Time
		trigger := func() {
			cfg, err := config.LoadGlobalOrDefault()
			if err != nil {
				log.Warn("adapter.config_reload_failed",
					"component", "adapter",
					"err", err,
				)
				return
			}
			if err := ctrl.apply(ctx, launchConfigFromGlobal(cfg), false, nil); err != nil {
				log.Warn("adapter.config_reload_apply_failed",
					"component", "adapter",
					"err", err,
				)
			}
		}

		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if !isAdapterConfigEvent(event, tomlPath) {
					continue
				}
				log.Debug("adapter.config_watch.event",
					"component", "adapter",
					"name", event.Name,
					"op", event.Op.String(),
				)
				if timer == nil {
					timer = time.NewTimer(adapterConfigReloadDebounce)
					timerCh = timer.C
					continue
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(adapterConfigReloadDebounce)
			case <-timerCh:
				timerCh = nil
				timer = nil
				trigger()
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Warn("adapter.config_watch.error",
					"component", "adapter",
					"err", err,
				)
			}
		}
	}()

	return func() {
		cancel()
		_ = watcher.Close()
		<-done
	}, nil
}

func isAdapterConfigEvent(event fsnotify.Event, tomlPath string) bool {
	name := filepath.Clean(event.Name)
	if name != tomlPath {
		return false
	}
	return event.Has(fsnotify.Write) ||
		event.Has(fsnotify.Create) ||
		event.Has(fsnotify.Remove) ||
		event.Has(fsnotify.Rename) ||
		event.Has(fsnotify.Chmod)
}

func (c *adapterController) apply(ctx context.Context, next adapterLaunchConfig, startup bool, inherited net.Listener) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := c.current
	if c.runtimeLogging == nil {
		c.runtimeLogging = adapter.NewRuntimeLogging(next.Logging)
	}
	c.deps.RuntimeLogging = c.runtimeLogging
	if !startup && adapterLaunchEquivalent(prev, next) {
		c.runtimeLogging.Set(next.Logging)
		if inherited != nil {
			_ = inherited.Close()
		}
		c.current = next
		c.log.Debug("adapter.config_reload.noop",
			"component", "adapter",
		)
		return nil
	}
	old := c.proc

	var srv *adapter.Server
	var err error
	if next.Enabled {
		srv, err = adapter.New(ctx, next.Adapter, next.Logging, c.deps, c.log)
		if err != nil {
			c.log.Error("adapter.registry.invalid_config",
				"component", "adapter",
				"err", err,
			)
			if startup {
				return fmt.Errorf("adapter registry: %w", err)
			}
			return nil
		}
		if inherited != nil {
			if got, want := inherited.Addr().String(), srv.Addr(); got != want {
				return fmt.Errorf("adapter inherited listener address %s does not match config %s; full daemon restart required", got, want)
			}
		}
	} else if inherited != nil {
		return fmt.Errorf("adapter listener inherited but adapter is disabled; full daemon restart required")
	}

	if old != nil {
		stopAdapterProcess(old, adapterShutdownWait)
	}

	if !next.Enabled {
		c.runtimeLogging.Set(next.Logging)
		c.proc = nil
		c.current = next
		c.log.Info("adapter.config_reload.disabled",
			"component", "adapter",
		)
		return nil
	}

	lis := inherited
	if lis == nil {
		lis, err = net.Listen("tcp", srv.Addr())
		if err != nil {
			return fmt.Errorf("adapter listen %s: %w", srv.Addr(), err)
		}
	}
	proc := startAdapterProcess(ctx, c.log, srv, lis)
	c.runtimeLogging.Set(next.Logging)
	c.proc = proc
	c.current = next
	c.log.Info("adapter.config_reload.applied",
		"component", "adapter",
		"enabled", next.Enabled,
		"host", next.Adapter.Host,
		"port", next.Adapter.Port,
		"default_model", next.Adapter.DefaultModel,
	)
	return nil
}

func adapterLaunchEquivalent(a, b adapterLaunchConfig) bool {
	a.Logging.Body = config.LoggingBody{}
	b.Logging.Body = config.LoggingBody{}
	return reflect.DeepEqual(a, b)
}

func (c *adapterController) listener() net.Listener {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proc == nil {
		return nil
	}
	return c.proc.lis
}

func (c *adapterController) drainReloadedProcess(timeout time.Duration) {
	c.mu.Lock()
	proc := c.proc
	c.mu.Unlock()
	if proc == nil || proc.drain == nil {
		return
	}
	c.log.Info("daemon.reload.draining_old_adapter",
		"component", "daemon",
		"addr", listenerAddr(proc.lis),
	)
	if proc.closeListener != nil {
		if err := proc.closeListener(); err != nil && !errors.Is(err, net.ErrClosed) {
			c.log.Warn("daemon.reload.adapter_listener_close_failed",
				"component", "daemon",
				"addr", listenerAddr(proc.lis),
				"err", err,
			)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	finalActive := 0
	if proc.waitIdle != nil {
		finalActive = proc.waitIdle(ctx)
	} else if proc.activeCount != nil {
		finalActive = proc.activeCount()
	}
	if finalActive == 0 {
		cancel()
		if proc.forceClose != nil {
			if err := proc.forceClose(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				c.log.Warn("daemon.reload.adapter_idle_force_close_failed",
					"component", "daemon",
					"addr", listenerAddr(proc.lis),
					"err", err,
				)
			}
		}
		c.log.Info("daemon.reload.adapter_drain_complete",
			"component", "daemon",
			"addr", listenerAddr(proc.lis),
			"active_requests", 0,
		)
		return
	}
	err := proc.drain(ctx)
	cancel()
	if err != nil {
		c.log.Warn("daemon.reload.adapter_drain_timeout",
			"component", "daemon",
			"addr", listenerAddr(proc.lis),
			"active_requests", finalActive,
			"err", err,
		)
	} else {
		c.log.Info("daemon.reload.adapter_drain_complete",
			"component", "daemon",
			"addr", listenerAddr(proc.lis),
			"active_requests", 0,
		)
	}
	if proc.forceClose != nil {
		if err := proc.forceClose(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			c.log.Warn("daemon.reload.adapter_force_close_failed",
				"component", "daemon",
				"addr", listenerAddr(proc.lis),
				"err", err,
			)
		} else if err != nil {
			c.log.Debug("daemon.reload.adapter_force_closed",
				"component", "daemon",
				"addr", listenerAddr(proc.lis),
			)
		}
	}
}

func (c *adapterController) stop() {
	c.mu.Lock()
	proc := c.proc
	c.proc = nil
	c.mu.Unlock()
	if proc != nil {
		stopAdapterProcess(proc, adapterShutdownWait)
	}
}

func stopAdapterProcess(proc *adapterProcess, timeout time.Duration) {
	if proc == nil {
		return
	}
	proc.cancel()
	select {
	case <-proc.done:
	case <-time.After(timeout):
	}
}

func startAdapterProcess(parent context.Context, log *slog.Logger, srv *adapter.Server, lis net.Listener) *adapterProcess {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Warn("adapter.run_panicked",
					"component", "adapter",
					"panic", r,
				)
			}
		}()
		defer close(done)
		if err := srv.StartOnListener(ctx, lis); err != nil {
			log.Error("adapter.exited",
				"component", "adapter",
				"err", err,
			)
		}
	}()
	return &adapterProcess{
		cancel:        cancel,
		drain:         srv.Shutdown,
		waitIdle:      srv.WaitForIdle,
		activeCount:   srv.ActiveRequestCount,
		forceClose:    srv.Close,
		closeListener: lis.Close,
		done:          done,
		lis:           lis,
	}
}
