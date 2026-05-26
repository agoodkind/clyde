package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
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
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/livetrack"
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

// reloadHTTPDrainWait caps how long the reload waits for the
// daemon-owned live-worker registry to drain before force-closing.
// The adapter, MITM proxy, and dashboard webapp use the longer
// reloadDrainCap via the shared runReloadDrain orchestrator so
// in-flight LLM streams survive reload to natural completion
// (CLYDE-324, CLYDE-437); only the live-worker drain still uses this
// short bound because workers should exit promptly when their leases
// release.
const (
	reloadHTTPDrainWait         = adapterShutdownWait
	reloadGRPCDrainWait         = 10 * time.Minute
	envDaemonReloadChild        = daemonsupervisor.EnvReloadChild
	envDaemonInheritedListeners = daemonsupervisor.EnvInheritedListeners
	envDaemonReadyFD            = daemonsupervisor.EnvReadyFD
	envDaemonSupervisorSocket   = daemonsupervisor.EnvSupervisorSocket
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
	// reloadDraining is set when drainReloadedProcess kicks off the
	// async adapter drain (1h cap) so the synchronous cancel path
	// (used by exclusive.stop on full daemon exit AND on reload
	// handoff) does not race and force-close in-flight HTTP/SSE
	// streams the async drain is letting finish naturally. Mirrors
	// the mitmProcess.reloadDraining gate established in CLYDE-324
	// and addressed for the adapter in CLYDE-437.
	reloadDraining atomic.Bool
}

type adapterController struct {
	log            *slog.Logger
	deps           adapter.Deps
	runtimeLogging *adapter.RuntimeLogging
	mu             sync.Mutex
	current        adapterLaunchConfig
	proc           *adapterProcess
	// srv is the current adapter.Server, retained so the daemon can reach the
	// adapter-owned OAuth rotation layer for RPC handlers. It is replaced on
	// each successful reload and nil when the adapter is disabled.
	srv *adapter.Server
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
	// srv holds the webapp Server so the reload chain can drain
	// srv.Channels directly, mirroring how mitmProcess.proxy exposes
	// proxy.Tunnels. The direct field access forces the
	// livetrack.Registry[webapp.WebMeta] type parameter to materialise
	// through a cross-package boundary, making WebMeta reachable.
	srv *webapp.Server
}

type mitmProcess struct {
	cancel        func()
	drain         func(context.Context) error
	waitIdle      func(context.Context) int
	activeCount   func() int
	forceClose    func() error
	closeListener func() error
	done          chan struct{}
	lis           net.Listener
	proxy         *mitm.Proxy
	// reloadDraining is set when drainReloadedMITM kicks off the
	// async drain (1h cap) so the synchronous cancel function (4s
	// cap, suitable for full daemon exit) does not race and
	// force-close in-flight HTTPS tunnels the async drain is letting
	// finish naturally. CLYDE-324.
	reloadDraining atomic.Bool
}

type daemonRuntime struct {
	listener   net.Listener
	adapter    *adapterController
	webapp     *webAppProcess
	mitm       *mitmProcess
	reloadLock sync.Mutex
	// drainDones holds a done channel per long-lived listener
	// surface the reload chain has handed off to a fire-and-forget
	// graceful drain (adapter HTTP/SSE, MITM tunnels, dashboard
	// webapp channels). The Run loop waits on every entry before
	// returning so the old worker process stays alive long enough
	// for in-flight streams to finish naturally rather than getting
	// truncated mid-stream by process exit (CLYDE-324, CLYDE-437).
	drainDonesMu sync.Mutex
	drainDones   map[string]<-chan struct{}
}

// setDrainDone records the done channel returned by runReloadDrain for
// one surface ("adapter", "mitm", "webapp") so the Run loop can wait on
// every active drain before letting the OS process exit. Replacing an
// existing entry is allowed because reload is gated by reloadLock and
// only one reload chain ever holds the runtime's drain channels at a
// time.
func (rt *daemonRuntime) setDrainDone(kind string, done <-chan struct{}) {
	if rt == nil || done == nil {
		return
	}
	rt.drainDonesMu.Lock()
	defer rt.drainDonesMu.Unlock()
	if rt.drainDones == nil {
		rt.drainDones = make(map[string]<-chan struct{})
	}
	rt.drainDones[kind] = done
}

// waitAllDrains blocks until every recorded drain channel is closed or
// the supplied cap elapses. Each surface emits its own drain_complete
// or drain_timeout event from inside the orchestrator; this helper only
// gates process exit and emits a single summary log line at the start
// plus one at the end naming any surfaces still pending when the cap
// elapsed.
func (rt *daemonRuntime) waitAllDrains(log *slog.Logger, drainCap time.Duration) {
	if rt == nil {
		return
	}
	rt.drainDonesMu.Lock()
	if len(rt.drainDones) == 0 {
		rt.drainDonesMu.Unlock()
		return
	}
	pending := make(map[string]<-chan struct{}, len(rt.drainDones))
	maps.Copy(pending, rt.drainDones)
	rt.drainDonesMu.Unlock()

	kinds := make([]string, 0, len(pending))
	for kind := range pending {
		kinds = append(kinds, kind)
	}
	log.Info("daemon.exit.waiting_drain",
		"component", "daemon",
		"kinds", kinds,
		"cap", drainCap.String(),
	)
	completed, lapsed := collectDrainResults(pending, drainCap)
	log.Info("daemon.exit.drain_summary",
		"component", "daemon",
		"completed", completed,
		"cap_elapsed", lapsed,
		"cap", drainCap.String(),
	)
}

// collectDrainResults blocks on every drain done channel up to
// drainCap and returns two slices: the kinds whose drains completed
// naturally before the cap elapsed, and the kinds still pending when
// the cap fired. The cap is shared across all surfaces because the
// daemon process cannot exit while any surface is still draining.
func collectDrainResults(pending map[string]<-chan struct{}, drainCap time.Duration) (completed []string, lapsed []string) {
	deadline := time.After(drainCap)
	completed = make([]string, 0, len(pending))
	for kind, done := range pending {
		select {
		case <-done:
			completed = append(completed, kind)
		case <-deadline:
			lapsed = append(lapsed, kind)
			for remainingKind := range pending {
				if remainingKind == kind {
					continue
				}
				select {
				case <-pending[remainingKind]:
					completed = append(completed, remainingKind)
				default:
					if remainingKind != kind {
						lapsed = append(lapsed, remainingKind)
					}
				}
			}
			return completed, lapsed
		}
	}
	return completed, lapsed
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
		listener:     listener,
		adapter:      subsystems.adapterCtrl,
		webapp:       subsystems.webProc,
		mitm:         subsystems.mitmProc,
		reloadLock:   sync.Mutex{},
		drainDonesMu: sync.Mutex{},
		drainDones:   nil,
	}

	exclusive := configureExclusiveSubsystems(log, reloadChild, extraLoops, subsystems.adapterCancel, subsystems.webProc, subsystems.mitmProc, processLock.lockAcquired)
	// rt.waitAllDrains blocks process exit until every fire-and-
	// forget per-surface drain goroutine (adapter HTTP/SSE, MITM
	// tunnels, dashboard webapp channels) finishes or its outer cap
	// elapses. Registered before exclusive.stop so its defer runs
	// AFTER exclusive.stop on return: stop signals each surface
	// (via its own Shutdown idempotency), then wait observes
	// natural completion. CLYDE-324, CLYDE-437: without this wait,
	// gRPC GracefulStop returns within seconds and process exit
	// kills the drain goroutines, truncating any LLM response in
	// flight (Cursor BYOK SSE, MITM-tunneled api.anthropic.com,
	// webapp dashboard channels).
	defer rt.waitAllDrains(log, reloadDrainCap)
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
	// Expose the single daemon-owned rotator to RPC handlers. It is the same
	// instance injected into the adapter's Deps and driven by the refresh loop,
	// so reads here observe the live serve/refresh state directly rather than
	// reaching through the reload-replaced adapter Server.
	srv.SetOAuthRotatorAccessor(SharedOAuthRotator)
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
	configureAutoNameWorker(log, srv)
	return daemonSubsystems{
		mitmProc:      mitmProc,
		adapterCtrl:   adapterCtrl,
		adapterCancel: adapterCancel,
		webProc:       webProc,
	}, nil
}

func configureAutoNameWorker(log *slog.Logger, srv *Server) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.Warn("daemon.autoname.config_load_failed",
			"component", "daemon",
			"subcomponent", "autoname",
			"err", err,
		)
		return
	}
	srv.configureAutoName(cfg.AutoName)
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
	reloadStart := daemonNow()
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
	log.InfoContext(ctx, "daemon.reload.replacement_requested",
		"component", "daemon",
		"new_pid", proc.pid,
		"elapsed_ms", daemonNow().Sub(reloadStart).Milliseconds(),
	)
	_ = readyWrite.Close()
	watchReplacementDaemon(ctx, log, proc)

	if err := waitForReplacementDaemon(ctx, readyRead); err != nil {
		_ = proc.kill()
		return reloadReport{}, err
	}
	log.InfoContext(ctx, "daemon.reload.replacement_ready",
		"component", "daemon",
		"new_pid", proc.pid,
		"elapsed_ms", daemonNow().Sub(reloadStart).Milliseconds(),
	)
	srv.preserveRuntimeDirsOnClose()
	drainReloadedPublicHTTP(ctx, log, rt)
	rt.setDrainDone("live_workers", startLiveWorkerReloadDrain(ctx, log, srv))
	log.InfoContext(ctx, "daemon.reload.async_drains_scheduled",
		"component", "daemon",
		"new_pid", proc.pid,
		"elapsed_ms", daemonNow().Sub(reloadStart).Milliseconds(),
	)
	if stopExclusive != nil {
		stopExclusive("reload_handoff")
	}
	log.InfoContext(ctx, "daemon.reload.exclusive_stop_complete",
		"component", "daemon",
		"new_pid", proc.pid,
		"elapsed_ms", daemonNow().Sub(reloadStart).Milliseconds(),
	)
	grpcDrainStarted := startReloadGRPCDrain(ctx, log, grpcServer, proc, srv)
	<-grpcDrainStarted
	if releaseProcessLock != nil {
		releaseProcessLock("reload_handoff")
	}
	log.InfoContext(ctx, "daemon.reload.process_lock_released",
		"component", "daemon",
		"new_pid", proc.pid,
		"elapsed_ms", daemonNow().Sub(reloadStart).Milliseconds(),
	)
	log.InfoContext(ctx, "daemon.reload.returning_report",
		"component", "daemon",
		"new_pid", proc.pid,
		"elapsed_ms", daemonNow().Sub(reloadStart).Milliseconds(),
	)
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

func startReloadGRPCDrain(ctx context.Context, log *slog.Logger, grpcServer *grpc.Server, proc *replacementDaemonProcess, srv *Server) <-chan struct{} {
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
		// Drain the RPC stream registry in parallel with gRPC's own
		// graceful stop so in-flight streams are accounted for and
		// force-closed if the deadline hits. CLYDE-437: the parent
		// ctx is the reload RPC's request context, which cancels the
		// moment reloadDaemonBinary returns to the gRPC handler.
		// Inheriting cancellation here made waitForIdle observe an
		// already-expired deadline and fire force_closed:1 with
		// duration_ms:0 even though reloadGRPCDrainWait is ten
		// minutes. context.WithoutCancel preserves correlation
		// fields while letting the drain run to its real deadline.
		drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), reloadGRPCDrainWait)
		defer drainCancel()
		if srv != nil && srv.RPCs != nil {
			rpcDrainResult := srv.RPCs.Drain(drainCtx, "grpc.reload")
			log.InfoContext(ctx, "daemon.reload.rpc_registry_drained",
				"component", "daemon",
				"new_pid", proc.pid,
				"final", rpcDrainResult.Final.String(),
				"remaining", rpcDrainResult.Remaining,
				"force_closed", rpcDrainResult.ForceClosed,
				"duration_ms", rpcDrainResult.Duration.Milliseconds(),
			)
		}
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
	cmd.Env = daemonsupervisor.EnvWithOverrides(os.Environ(),
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

func drainReloadedPublicHTTP(ctx context.Context, log *slog.Logger, rt *daemonRuntime) {
	if rt == nil {
		return
	}
	// Every long-lived listener subsystem hands its in-flight
	// drain off to a fire-and-forget goroutine via the shared
	// runReloadDrain orchestrator. The replacement daemon already
	// inherits each listener FD, so new connections land on it;
	// only the old worker's existing in-flight goroutines must
	// outlive this call. CLYDE-324, CLYDE-437: blocking the reload
	// RPC on a short force-close was truncating LLM streams
	// mid-flight (Cursor BYOK SSE getting the generic rate-limit
	// chrome, claude CLI sessions dying). Each done-channel is
	// stored on the runtime so the old worker's Run loop waits for
	// every surface to finish before exiting; without that wait,
	// process exit kills the drain goroutines and the bug returns.
	if rt.adapter != nil {
		rt.setDrainDone("adapter", rt.adapter.drainReloadedProcess(ctx, log))
	}
	rt.setDrainDone("mitm", drainReloadedMITM(ctx, log, rt))
	rt.setDrainDone("webapp", drainReloadedWebApp(ctx, log, rt))
}

// drainReloadedWebApp mirrors the adapter and MITM reload drain
// pattern for the dashboard webapp by delegating to the shared
// runReloadDrain orchestrator. Long-lived browser channels (SSE
// streams, websockets) tracked by srv.Channels survive reload up to
// reloadDrainCap before force-close fires, so dashboard panels do not
// disconnect mid-stream during a `make deploy` (CLYDE-437).
//
// Accessing srv.Channels from the daemon package forces the
// livetrack.Registry[webapp.WebMeta] type parameter to materialise
// through a cross-package boundary, keeping WebMeta.IsLivetrackMeta
// reflection-reachable to the deadcode analyser (CLYDE-277).
func drainReloadedWebApp(ctx context.Context, log *slog.Logger, rt *daemonRuntime) <-chan struct{} {
	done := make(chan struct{})
	if rt == nil || rt.webapp == nil || rt.webapp.drain == nil {
		close(done)
		return done
	}
	wp := rt.webapp
	addr := listenerAddr(wp.lis)
	return runReloadDrain(ctx, reloadDrainSurface{
		Kind:           "webapp",
		Log:            log,
		Addr:           addr,
		ActiveField:    "active_channels",
		ReloadDraining: nil,
		CloseListener:  wp.closeListener,
		ActiveCount:    webappChannelActiveCount(wp),
		Drain:          buildWebappReloadDrain(log, addr, wp),
		ForceClose:     wp.forceClose,
	})
}

// webappChannelActiveCount exposes the per-process Channels registry
// count so the shared orchestrator can fast-path the idle case and
// report active_channels on drain events. Returns 0 when the webapp
// process or its server is nil.
func webappChannelActiveCount(wp *webAppProcess) func() int {
	return func() int {
		if wp == nil || wp.srv == nil || wp.srv.Channels == nil {
			return 0
		}
		return wp.srv.Channels.Count()
	}
}

// buildWebappReloadDrain returns the surface's drain implementation.
// It first drains the Channels registry so SSE handlers see the drain
// signal before [http.Server.Shutdown] stops accepting, then runs
// the underlying server shutdown against the same ctx. The
// orchestrator passes a fresh background-derived ctx with
// reloadDrainCap so neither step is short-circuited by the reload
// RPC's request context cancelling on return. Channel drain errors
// are logged before being returned so operators see which channels
// failed to release.
func buildWebappReloadDrain(log *slog.Logger, addr string, wp *webAppProcess) func(context.Context) error {
	return func(ctx context.Context) error {
		if wp == nil {
			return nil
		}
		if wp.srv != nil && wp.srv.Channels != nil {
			result := wp.srv.Channels.DrainWith(ctx, "webapp.reload", livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace})
			if len(result.Errors) > 0 {
				err := errors.Join(result.Errors...)
				log.Warn("daemon.reload.webapp_channels_drain_errors",
					"component", "daemon",
					"addr", addr,
					"err", err,
				)
				return err
			}
		}
		if wp.drain == nil {
			return nil
		}
		return wp.drain(ctx)
	}
}

// drainReloadedMITM closes the old worker's MITM listener
// synchronously so the replacement daemon owns the accept loop, then
// hands the in-flight tunnel drain off to a goroutine bounded by
// reloadDrainCap via the shared runReloadDrain orchestrator. The
// returned channel is closed when the drain goroutine exits; the
// reload RPC ignores it so handoff returns promptly while the old
// worker keeps forwarding bytes for any HTTPS tunnel that was
// mid-stream when reload fired (CLYDE-324, CLYDE-437).
//
// The shared orchestrator owns the listener-close, idle-fast-path
// force-close, active-path goroutine, panic recovery, and event
// emission so the same drain shape is used uniformly across the
// adapter, MITM, and webapp surfaces.
func drainReloadedMITM(ctx context.Context, log *slog.Logger, rt *daemonRuntime) <-chan struct{} {
	done := make(chan struct{})
	if rt == nil || rt.mitm == nil {
		close(done)
		return done
	}
	mitmProc := rt.mitm
	return runReloadDrain(ctx, reloadDrainSurface{
		Kind:           "mitm",
		Log:            log,
		Addr:           listenerAddr(mitmProc.lis),
		ActiveField:    "active_tunnels",
		ReloadDraining: &mitmProc.reloadDraining,
		CloseListener:  mitmProc.closeListener,
		ActiveCount:    mitmProc.activeCount,
		Drain:          mitmProc.drain,
		ForceClose:     mitmProc.forceClose,
	})
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
	// Log a startup record that includes the channel meta type as a slog
	// value. Passing webapp.WebMeta{} to slog boxes it into any, which
	// creates a MakeInterface instruction for webapp.WebMeta in the SSA
	// graph. This makes WebMeta.IsLivetrackMeta reflection-reachable to the
	// deadcode analyser, matching how MCP and MITM keep their meta types
	// live (CLYDE-270, CLYDE-277).
	log.Debug("webapp.starting",
		"component", "webapp",
		"addr", srvW.Addr(),
		"channel_meta", webapp.WebMeta{
			ChannelKind: "sse",
			ClientID:    "",
			RemoteAddr:  "",
			Subscribed:  nil,
		},
	)
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
		srv:           srvW,
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
	proxy, err := mitm.NewProxy(cfg.MITM, cfg.Logging.Request, log, lis)
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
	proc := &mitmProcess{
		cancel:         nil,
		drain:          mitmReloadDrain(proxy),
		waitIdle:       mitmTunnelWaitIdle(proxy),
		activeCount:    mitmTunnelActiveCount(proxy),
		forceClose:     mitmTunnelForceClose(proxy),
		closeListener:  lis.Close,
		done:           done,
		lis:            lis,
		proxy:          proxy,
		reloadDraining: atomic.Bool{},
	}
	// CLYDE-324: cancel is called by exclusive.stop on full daemon
	// exit (Ctrl+C, SIGTERM) AND on reload handoff. The reload path
	// already kicks off an async drain with the long cap; if cancel
	// also calls Shutdown with the short adapterShutdownWait it races
	// the async drain and force-closes in-flight HTTPS tunnels. The
	// reloadDraining flag flips to true at the top of
	// drainReloadedMITM, so the reload-time cancel becomes a no-op
	// while the full-exit cancel keeps its short, deterministic
	// shutdown.
	proc.cancel = func() {
		if proc.reloadDraining.Load() {
			log.Debug("mitm.cancel.skipped_reload_drain",
				"component", "mitm",
			)
			return
		}
		ctx, cancelTimeout := context.WithTimeout(context.Background(), adapterShutdownWait)
		defer cancelTimeout()
		if err := proxy.Shutdown(ctx); err != nil {
			log.Warn("mitm.shutdown_failed",
				"component", "mitm",
				"err", err,
			)
		}
	}
	return proc, nil
}

// mitmTunnelActiveCount returns a closure that reports the current
// tunnel count; it is the MITM analogue of
// adapter.Server.ActiveRequestCount.
func mitmTunnelActiveCount(proxy *mitm.Proxy) func() int {
	return func() int {
		if proxy == nil || proxy.Tunnels == nil {
			return 0
		}
		return proxy.Tunnels.Count()
	}
}

// mitmTunnelWaitIdle polls the tunnel registry's count until it
// reaches zero or ctx fires. Polling cadence (50ms) matches the
// adapter Server.WaitForIdle so reload drain timing is symmetric
// across the two HTTP surfaces. Returns the final count when ctx
// fires.
func mitmTunnelWaitIdle(proxy *mitm.Proxy) func(context.Context) int {
	return func(ctx context.Context) int {
		if proxy == nil || proxy.Tunnels == nil {
			return 0
		}
		if proxy.Tunnels.Count() == 0 {
			return 0
		}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return proxy.Tunnels.Count()
			case <-ticker.C:
				if proxy.Tunnels.Count() == 0 {
					return 0
				}
			}
		}
	}
}

// mitmReloadDrain returns the drain function the reload orchestrator
// invokes for the MITM surface. It wraps [mitm.Proxy.ShutdownWith]
// so the registry's idle-grace fast-path force-closes wedged
// keepalive tunnels at drain start instead of waiting them out
// against the outer cap. Plain shutdown (non-reload) callers still
// use [mitm.Proxy.Shutdown], which uses the zero-grace path so
// existing tests and the full-exit path keep their pre-grace
// semantics.
func mitmReloadDrain(proxy *mitm.Proxy) func(context.Context) error {
	return func(ctx context.Context) error {
		if proxy == nil {
			return nil
		}
		return proxy.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace})
	}
}

// mitmTunnelForceClose drains the registry against a fresh
// background-derived context so an already-expired drain ctx does
// not block force-close. The composite error is logged before being
// returned so operators see exactly which tunnels failed to close.
func mitmTunnelForceClose(proxy *mitm.Proxy) func() error {
	return func() error {
		if proxy == nil || proxy.Tunnels == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), adapterShutdownWait)
		defer cancel()
		result := proxy.Tunnels.Drain(ctx, "mitm.force_close")
		if len(result.Errors) == 0 {
			return nil
		}
		err := errors.Join(result.Errors...)
		slog.Warn("daemon.reload.mitm_force_close_errors",
			"component", "daemon",
			"err", err,
		)
		return err
	}
}

// drainReloadedLiveWorkers drains the daemon-owned live-worker registry during
// a binary reload. Workers that exit within the drain window release naturally;
// workers still alive at the deadline are force-closed so the replacement
// daemon owns a clean slate. The drain runs against a short-lived context so
// a wedged worker cannot block the reload indefinitely.
func startLiveWorkerReloadDrain(ctx context.Context, log *slog.Logger, srv *Server) <-chan struct{} {
	if srv == nil || srv.liveWorkers == nil {
		return nil
	}
	remaining := srv.liveWorkers.Count()
	if remaining == 0 {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "daemon.reload.live_workers_drain_panicked",
					"component", "daemon",
					"panic", r,
				)
			}
		}()
		drainReloadedLiveWorkers(ctx, log, srv)
	}()
	return done
}

func drainReloadedLiveWorkers(ctx context.Context, log *slog.Logger, srv *Server) {
	if srv == nil || srv.liveWorkers == nil {
		return
	}
	remaining := srv.liveWorkers.Count()
	if remaining == 0 {
		return
	}
	log.InfoContext(ctx, "daemon.reload.draining_live_workers",
		"component", "daemon",
		"count", remaining,
	)
	drainCtx, cancel := context.WithTimeout(context.Background(), reloadHTTPDrainWait)
	result := srv.liveWorkers.Drain(drainCtx, "daemon.reload")
	cancel()
	if result.ForceClosed > 0 {
		log.WarnContext(ctx, "daemon.reload.live_workers_force_closed",
			"component", "daemon",
			"force_closed", result.ForceClosed,
			"duration_ms", result.Duration.Milliseconds(),
		)
	} else {
		log.InfoContext(ctx, "daemon.reload.live_workers_drain_complete",
			"component", "daemon",
			"force_closed", result.ForceClosed,
			"duration_ms", result.Duration.Milliseconds(),
		)
	}
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
		provider:         session.ProviderCodex,
		name:             attached.ThreadID,
		id:               attached.ThreadID,
		basedir:          attached.WorkDir,
		model:            attached.Model,
		status:           "attached",
		startedAt:        daemonNow(),
		lastTurnID:       "",
		codexRuntime:     runtime,
		livetrackSession: nil,
	}
	s.liveSessions[live.id] = live
	s.remoteMu.Unlock()
	lsess, ltrackErr := s.liveWorkers.Register(ctx, "codex.live", LiveMeta{
		Provider:      "codex",
		LiveSessionID: live.id,
		WorkerPID:     0,
		Lease:         "background",
	}, &codexRuntimeCloser{runtime: runtime})
	if ltrackErr == nil {
		s.remoteMu.Lock()
		live.livetrackSession = lsess
		s.remoteMu.Unlock()
	}
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

	// Build the single, daemon-owned OAuth rotation layer once and share it
	// with both the adapter (serve + throttle) and the in-process refresh loop
	// (harvest + due-check). One instance means an on-disk token the refresh
	// loop renews is visible to the adapter's serve path through shared
	// in-memory slots, fixing the stale-token window the two-instance wiring
	// had. The instance is retained on the controller's Deps so adapter reloads
	// reuse it rather than constructing a new one.
	//
	// The MITM tracker feeds the in-use detector the live count of active
	// claude-to-anthropic MITM sessions so the rotator skips refresh for an
	// account currently held by Claude Code traffic flowing through the proxy.
	mitmTracker := newAnthropicMITMTracker(func() *mitm.Proxy {
		if mitmProc == nil {
			return nil
		}
		return mitmProc.proxy
	})
	oauthRotator := buildDaemonOAuthRotator(context.Background(), cfg.Adapter, log, mitmTracker)
	setSharedOAuthRotator(oauthRotator)

	// current starts as the zero launch config; apply overwrites it on the
	// first call below. A var (not a composite literal) avoids the exhaustruct
	// gate firing on the nested empty config structs.
	var zeroLaunch adapterLaunchConfig
	ctrl := &adapterController{
		log:            log,
		runtimeLogging: adapter.NewRuntimeLogging(cfg.Logging),
		mu:             sync.Mutex{},
		current:        zeroLaunch,
		proc:           nil,
		srv:            nil,
		deps: adapter.Deps{
			ResolveClaude:                findRealClaude,
			ScratchDir:                   adapterScratchDir,
			RequestEvents:                srv.providerStats.Record,
			AnthropicMessagesURLOverride: mitmOverride,
			OAuthRotator:                 oauthRotator,
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
		c.srv = nil
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
	c.srv = srv
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

// drainReloadedProcess hands the adapter's in-flight HTTP/SSE drain
// off to the shared runReloadDrain orchestrator. The listener is
// closed synchronously so new connections land on the replacement
// daemon's inherited socket; the active-path drain runs in a
// background goroutine bounded by reloadDrainCap so in-flight
// /v1/chat/completions SSE streams finish naturally instead of being
// truncated mid-stream (CLYDE-437).
//
// Returns a done channel the daemon Run loop waits on so the OS
// process stays alive until the drain signals done.
func (c *adapterController) drainReloadedProcess(ctx context.Context, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	c.mu.Lock()
	proc := c.proc
	c.mu.Unlock()
	if proc == nil || proc.drain == nil {
		close(done)
		return done
	}
	if log == nil {
		log = c.log
	}
	return runReloadDrain(ctx, reloadDrainSurface{
		Kind:           "adapter",
		Log:            log,
		Addr:           listenerAddr(proc.lis),
		ActiveField:    "active_requests",
		ReloadDraining: &proc.reloadDraining,
		CloseListener:  proc.closeListener,
		ActiveCount:    proc.activeCount,
		Drain:          proc.drain,
		ForceClose:     adapterForceCloseLogging(log, listenerAddr(proc.lis), proc.forceClose),
	})
}

// adapterForceCloseLogging wraps the adapter's forceClose to preserve
// the prior log shape: warn on a non-ErrServerClosed / non-ErrClosed
// error (genuine close failure), debug on ErrServerClosed (the server
// already shut down). Other surfaces (MITM, webapp) use the shared
// debug log inside the orchestrator instead.
func adapterForceCloseLogging(log *slog.Logger, addr string, inner func() error) func() error {
	if inner == nil {
		return nil
	}
	return func() error {
		err := inner()
		if err == nil {
			return nil
		}
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			log.Debug("daemon.reload.adapter_force_closed",
				"component", "daemon",
				"addr", addr,
			)
			return nil
		}
		log.Warn("daemon.reload.adapter_force_close_failed",
			"component", "daemon",
			"addr", addr,
			"err", err,
		)
		return err
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
	if proc.reloadDraining.Load() {
		return
	}
	select {
	case <-proc.done:
	case <-time.After(timeout):
	}
}

// adapterReloadDrain returns the drain function the reload
// orchestrator invokes for the adapter surface. It wraps
// [adapter.Server.ShutdownWith] so the ingress and egress registries
// each apply the daemon's idle-grace fast-path: wedged ingress
// sessions that have not moved a byte since drain start get
// force-closed immediately, while sessions still streaming SSE ride
// out the outer cap. The full-exit shutdown path keeps using
// [adapter.Server.Shutdown] so non-reload callers retain the
// zero-grace semantics.
func adapterReloadDrain(adapterSrv *adapter.Server) func(context.Context) error {
	return func(ctx context.Context) error {
		if adapterSrv == nil {
			return nil
		}
		return adapterSrv.ShutdownWith(ctx, livetrack.DrainOptions{IdleGrace: reloadDrainIdleGrace})
	}
}

func startAdapterProcess(parent context.Context, log *slog.Logger, adapterSrv *adapter.Server, lis net.Listener) *adapterProcess {
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
		if err := adapterSrv.StartOnListener(ctx, lis); err != nil {
			log.Error("adapter.exited",
				"component", "adapter",
				"err", err,
			)
		}
	}()
	proc := &adapterProcess{
		cancel:         nil,
		drain:          adapterReloadDrain(adapterSrv),
		waitIdle:       adapterSrv.WaitForIdle,
		activeCount:    adapterSrv.ActiveRequestCount,
		forceClose:     adapterSrv.Close,
		closeListener:  lis.Close,
		done:           done,
		lis:            lis,
		reloadDraining: atomic.Bool{},
	}
	// CLYDE-437: cancel is called by exclusive.stop on full daemon
	// exit AND on reload handoff. The reload path already kicks off
	// an async drain with reloadDrainCap; if cancel also cancels
	// the adapter context (which triggers StartOnListener's select
	// to fire [adapter.Server.Shutdown] with a short 3s timeout) it
	// races the async drain and force-closes in-flight SSE streams.
	// The reloadDraining flag flips to true at the top of
	// drainReloadedProcess via runReloadDrain so the reload-time
	// cancel becomes a no-op while the full-exit cancel keeps its
	// short, deterministic shutdown. Mirrors the MITM gate from
	// CLYDE-324.
	proc.cancel = func() {
		if proc.reloadDraining.Load() {
			log.Debug("adapter.cancel.skipped_reload_drain",
				"component", "adapter",
			)
			return
		}
		cancel()
	}
	return proc
}
