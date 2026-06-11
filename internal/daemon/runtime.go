package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
	searchstore "goodkind.io/clyde/internal/search/store"
)

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
	// searchStore is the SQLite store for async conversation search jobs, and
	// searchJobs is the manager that runs them. The store is closed and the
	// manager drained on reload and shutdown so in-flight jobs stop and the WAL
	// flushes before a new generation reopens the same db.
	searchStore *searchstore.Store
	searchJobs  *conversation.SearchJobManager
	semantic    *conversationSemanticRuntime
	// pprofListener is the optional loopback pprof socket. It is nil when pprof
	// is off. When set, it is inherited across reload like the adapter and MITM
	// listeners so the debug surface survives a hot reload with no bind gap.
	pprofListener net.Listener
	errors        chan error
	reloadMu      sync.Mutex
	reloadDrain   <-chan struct{}
	// configWatcher watches the config file and triggers an automatic reload
	// when it changes. It is nil when config auto-reload has not started.
	configWatcher *configWatcher
	// group is the single lifecycle plan every long-lived registry attaches to.
	// Quiesce drives reload and shutdown drains; AwaitQuiet backs the watcher's
	// quiet-wait. Constructed in startRuntime before any subsystem.
	group *livetrack.Group
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
	searchStore, err := openSearchStore(ctx, log)
	if err != nil {
		_ = listener.Close()
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
		searchStore:           searchStore,
		searchJobs:            nil,
		semantic:              nil,
		pprofListener:         nil,
		errors:                make(chan error, 3),
		reloadMu:              sync.Mutex{},
		reloadDrain:           nil,
		configWatcher:         nil,
		group:                 newLifecycleGroup(log),
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
		server, adapterListener, adapterCursorListener, err := startAdapter(ctx, cfg, log, stats, runtime.captureStore, runtime.group, runtime.errors, inherited.listeners[listenerNameAdapter], inherited.listeners[listenerNameAdapterCursor])
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
	runtime.semantic = startConversationSemanticRuntime(ctx, cfg, log)
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
	proxy, err := mitm.NewProxy(cfg.MITM, cfg.Logging.Request, log, sockets, runtime.captureStore, listenerCfg.ID, runtime.group)
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
	group *livetrack.Group,
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
		Group:                     group,
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

// stopConfigWatcher cancels and drains the config watcher if one is running. It
// is nil-safe and used on the shutdown path where the loop is idle.
func (r *runtimeServices) stopConfigWatcher(ctx context.Context, reason string) {
	if r == nil || r.configWatcher == nil {
		return
	}
	r.configWatcher.stop(ctx, reason)
}

// cancelConfigWatcher cancels the config watcher loop without waiting. It is
// nil-safe and used by reload and rebind, where the watcher may be the in-flight
// caller and a blocking drain would deadlock.
func (r *runtimeServices) cancelConfigWatcher() {
	if r == nil || r.configWatcher == nil {
		return
	}
	r.configWatcher.cancelLoop()
}

func (r *runtimeServices) shutdown(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, daemonShutdownTimeout)
	defer cancel()
	if r == nil {
		return
	}
	r.stopConfigWatcher(ctx, "shutdown")
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
	// Drain in-flight search jobs and close their store so the SQLite WAL
	// flushes before the process exits.
	if r.searchJobs != nil {
		r.searchJobs.Drain(ctx, "shutdown")
	}
	if r.searchStore != nil {
		if err := r.searchStore.Close(parent, "shutdown"); err != nil {
			slog.WarnContext(ctx, "daemon.shutdown.search_store_close_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		}
	}
	r.closeConversationSemanticRuntime(ctx, slog.Default(), "shutdown")
	// Close the shared capture store after the proxies stop so the SQLite WAL
	// flushes before the process exits.
	if r.captureStore != nil {
		if err := r.captureStore.Close(parent, "shutdown"); err != nil {
			slog.WarnContext(ctx, "daemon.shutdown.capture_store_close_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		}
	}
}
