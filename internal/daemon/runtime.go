package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"

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
	"goodkind.io/clyde/internal/providerid"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	"goodkind.io/clyde/internal/reorientinject"
	"goodkind.io/clyde/internal/sentinelinject"
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
	// entries. captureStore is the single SQLite capture sink shared by the
	// adapter and every proxy; it is closed after both surfaces drain so the WAL
	// flushes before any new generation reopens it.
	mitmProxies     map[string]*mitm.Proxy
	mitmListeners   map[string][]net.Listener
	mitmPacketConns map[string][]net.PacketConn
	captureStore    *capture.Store
	semantic        *conversationSemanticRuntime
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
	// stats owns the in-process provider activity and pricing state. The hot
	// apply path replaces its immutable pricing table after every fallible
	// component apply succeeds.
	stats *providerStatsRecorder
	// currentConfig holds the config the running generation is serving. The
	// in-process apply path compares against it to classify a change and
	// advances it after a successful hot apply.
	currentConfig atomic.Pointer[config.Config]
}

type inheritedRuntime struct {
	listeners   map[string]net.Listener
	packetConns map[string]net.PacketConn
	ready       *os.File
}

func startRuntime(
	ctx context.Context,
	cfg *config.Config,
	log *slog.Logger,
	stats *providerStatsRecorder,
	inherited inheritedRuntime,
) (*runtimeServices, error) {
	listener, err := daemonListener(ctx, cfg.Daemon.GRPCAddress, inherited.listeners[listenerNameDaemon])
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
		mitmPacketConns:       map[string][]net.PacketConn{},
		captureStore:          nil,
		semantic:              nil,
		pprofListener:         nil,
		errors:                make(chan error, 3),
		reloadMu:              sync.Mutex{},
		reloadDrain:           nil,
		configWatcher:         nil,
		group:                 newLifecycleGroup(log),
		stats:                 stats,
		currentConfig:         atomic.Pointer[config.Config]{},
	}
	runtime.currentConfig.Store(cfg)
	if captureStoreRequired(cfg) {
		if err := startCaptureStore(ctx, cfg, log, runtime); err != nil {
			runtime.shutdown(context.WithoutCancel(ctx))
			return nil, err
		}
	}
	if cfg.MITM.EnabledDefault {
		if err := startMITM(ctx, cfg, log, runtime, inherited); err != nil {
			runtime.shutdown(context.WithoutCancel(ctx))
			return nil, err
		}
	} else if hasInheritedMITMListener(inherited) {
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
		runtime.addAdapterLifecycleHooks(server)
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
	runtime.semantic = startConversationSemanticRuntime(ctx, cfg, log, runtime.group)
	return runtime, nil
}

func captureStoreRequired(cfg *config.Config) bool {
	return cfg.MITM.EnabledDefault || (cfg.Adapter.Enabled && cfg.Adapter.CaptureIngress)
}

func startCaptureStore(ctx context.Context, cfg *config.Config, log *slog.Logger, runtime *runtimeServices) error {
	store, err := openMITMCaptureStore(ctx, cfg, log)
	if err != nil {
		log.WarnContext(ctx, "daemon.capture_store_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"db_path", cfg.MITM.CaptureStore.DBPath,
			"err", err,
		)
		return err
	}
	runtime.captureStore = store
	runtime.addCaptureStoreCloseHook(store)
	return nil
}

// startMITM starts one proxy per configured listener against the shared store.
// On failure the caller drains the proxies and store already started.
func startMITM(ctx context.Context, cfg *config.Config, log *slog.Logger, runtime *runtimeServices, inherited inheritedRuntime) error {
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
// closeMITMSockets closes any TCP listeners and UDP packet connections a
// partially-started MITM listener bound before an error aborted startup.
func closeMITMSockets(sockets []net.Listener, packetConns []net.PacketConn) {
	for _, socket := range sockets {
		_ = socket.Close()
	}
	for _, conn := range packetConns {
		_ = conn.Close()
	}
}

// bindMITMStreamListeners binds (or adopts inherited) TCP listeners for every
// address the listener id expands to. It closes any it bound before returning
// an error, so the caller never leaks a half-bound set.
func bindMITMStreamListeners(ctx context.Context, log *slog.Logger, listenerCfg config.MITMListenerConfig, addrs []string, inherited inheritedRuntime) ([]net.Listener, error) {
	sockets := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		if existing := inherited.listeners[mitmSocketKey(listenerCfg.ID, addr)]; existing != nil {
			if got := existing.Addr().String(); got != addr {
				closeMITMSockets(sockets, nil)
				return nil, fmt.Errorf("mitm listener %q inherited address %s does not match config %s; full daemon restart required", listenerCfg.ID, got, addr)
			}
			sockets = append(sockets, existing)
			continue
		}
		listenConfig := net.ListenConfig{}
		bound, err := listenConfig.Listen(ctx, "tcp", addr)
		if err != nil {
			closeMITMSockets(sockets, nil)
			log.WarnContext(ctx, "daemon.mitm.listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"listener_id", listenerCfg.ID,
				"addr", addr,
				"err", err,
			)
			return nil, fmt.Errorf("mitm listen %s for listener %q: %w", addr, listenerCfg.ID, err)
		}
		sockets = append(sockets, bound)
	}
	return sockets, nil
}

// bindMITMPacketConns binds (or adopts inherited) UDP packet connections for
// the same addresses, carrying HTTP/3 traffic on the listener id's port. It
// closes any it bound before returning an error.
func bindMITMPacketConns(ctx context.Context, log *slog.Logger, listenerCfg config.MITMListenerConfig, addrs []string, inherited inheritedRuntime) ([]net.PacketConn, error) {
	packetConns := make([]net.PacketConn, 0, len(addrs))
	for _, addr := range addrs {
		if existing := inherited.packetConns[mitmUDPSocketKey(listenerCfg.ID, addr)]; existing != nil {
			if got := existing.LocalAddr().String(); got != addr {
				closeMITMSockets(nil, packetConns)
				return nil, fmt.Errorf("mitm udp listener %q inherited address %s does not match config %s; full daemon restart required", listenerCfg.ID, got, addr)
			}
			packetConns = append(packetConns, existing)
			continue
		}
		listenConfig := net.ListenConfig{}
		bound, err := listenConfig.ListenPacket(ctx, "udp", addr)
		if err != nil {
			closeMITMSockets(nil, packetConns)
			log.WarnContext(ctx, "daemon.mitm.quic_listen_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
				"listener_id", listenerCfg.ID,
				"addr", addr,
				"err", err,
			)
			return nil, fmt.Errorf("mitm udp listen %s for listener %q: %w", addr, listenerCfg.ID, err)
		}
		packetConns = append(packetConns, bound)
	}
	return packetConns, nil
}

// mitmRequestResponseHooks returns the MITM request/response hooks enabled by
// config. The default configuration registers no hooks and the proxy path stays
// byte-for-byte unchanged. Sentinel is registered first so a matched sentinel
// rewrite wins over reorient when both are enabled and both would match.
func mitmRequestResponseHooks(mitmCfg config.MITMConfig) []mitm.RequestResponseHook {
	var hooks []mitm.RequestResponseHook
	sentinel := strings.TrimSpace(mitmCfg.Sentinel)
	actualUserSentinel := strings.TrimSpace(mitmCfg.ActualUserSentinel)
	if sentinel != "" || actualUserSentinel != "" {
		hooks = append(hooks, sentinelinject.New(sentinel, actualUserSentinel))
	}
	if mitmCfg.ReorientSummaryInjection {
		hooks = append(hooks, reorientinject.New(
			newReorientInjectContentProvider(mitmCfg.ReorientInjectMaxLines),
			reorientinject.Sizing{
				MaxTokens:               mitmCfg.ReorientInjectMaxTokens,
				ContextWindowFraction:   mitmCfg.ReorientContextWindowFraction,
				BytesPerToken:           mitmCfg.ReorientBytesPerToken,
				StandardContextWindow:   mitmCfg.ReorientStandardContextWindow,
				OneMillionContextWindow: mitmCfg.ReorientOneMillionContextWindow,
				RecentFraction:          mitmCfg.ReorientRecentFraction,
			},
		))
	}
	return hooks
}

// newReorientInjectContentProvider builds the reorient content provider. It
// closes over a dedicated conversation index used only to render a transcript
// directly from its on-disk path: the index never scans and is independent of
// the daemon's shared index and its refresh cycle. Given the session id parsed
// from the intercepted compaction request, the provider resolves the Claude
// transcript file and renders the recovered pre-compaction transcript off disk
// with clyde's reorient knobs (tool outputs, dense, line-capped). maxLines caps
// the recovered transcript; zero uses the conversation renderer default. An empty
// or unresolvable session id yields empty content, which passes the response
// through unchanged.
func newReorientInjectContentProvider(maxLines int) reorientinject.ContentProvider {
	index := NewConversationIndex()
	return func(ctx context.Context, sessionID string, maxBytes int) (string, error) {
		if sessionID == "" {
			return "", nil
		}
		// Honor cancellation (client disconnect or timeout) before the filesystem
		// walk and the render, so a canceled compaction does not keep scanning
		// ~/.claude/projects or rendering a large transcript. The transformer
		// treats a provider error as pass-through, so injection is skipped.
		if err := ctx.Err(); err != nil {
			slog.WarnContext(ctx, "daemon.reorient_inject.canceled", "concern", "providers.mitm.wire", "component", "daemon", "err", err)
			return "", fmt.Errorf("reorient inject canceled: %w", err)
		}
		path, ok := claudeparser.TranscriptPathForSession(sessionID)
		if !ok {
			return "", nil
		}
		return index.RenderReorientArtifact(path, providerid.ProviderClaude, conversation.ReorientOptions{
			ConversationID:      "",
			WorkspaceRoot:       "",
			MaxLines:            maxLines,
			MaxBytes:            maxBytes,
			IncludeToolOutputs:  true,
			SyntheticPreCompact: true,
		})
	}
}

func startMITMListener(ctx context.Context, cfg *config.Config, log *slog.Logger, runtime *runtimeServices, listenerCfg config.MITMListenerConfig, inherited inheritedRuntime) error {
	addrs := mitmBindAddrs(listenerCfg.Host, listenerCfg.Port)
	sockets, err := bindMITMStreamListeners(ctx, log, listenerCfg, addrs, inherited)
	if err != nil {
		return err
	}
	packetConns, err := bindMITMPacketConns(ctx, log, listenerCfg, addrs, inherited)
	if err != nil {
		closeMITMSockets(sockets, nil)
		return err
	}
	proxy, err := mitm.NewProxy(cfg.MITM, cfg.Logging.Request, log, sockets, packetConns, runtime.captureStore, listenerCfg.ID, runtime.group)
	if err != nil {
		closeMITMSockets(sockets, packetConns)
		log.WarnContext(ctx, "daemon.mitm.init_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"listener_id", listenerCfg.ID,
			"addrs", addrs,
			"err", err,
		)
		return fmt.Errorf("init mitm proxy for listener %q: %w", listenerCfg.ID, err)
	}
	proxy.SetRequestResponseHooks(mitmRequestResponseHooks(cfg.MITM))
	runtime.mitmProxies[listenerCfg.ID] = proxy
	runtime.mitmListeners[listenerCfg.ID] = sockets
	runtime.mitmPacketConns[listenerCfg.ID] = packetConns
	runtime.addMITMLifecycleHook(listenerCfg.ID, proxy)
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
		if serveErr := proxy.Serve(ctx); serveErr != nil {
			errCh <- serveErr
		}
	}()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "daemon.mitm.quic_serve_panic", "concern", "process.daemon.lifecycle", "component", "daemon",
					"listener_id", listenerCfg.ID,
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		if serveErr := proxy.ServeQUIC(ctx); serveErr != nil {
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
func hasInheritedMITMListener(inherited inheritedRuntime) bool {
	for name := range inherited.listeners {
		if strings.HasPrefix(name, listenerNameMITMPrefix) {
			return true
		}
	}
	for name := range inherited.packetConns {
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
		ScratchDir:     ensureScratchDir,
		RequestEvents:  stats.record,
		RuntimeLogging: adapter.NewRuntimeLogging(cfg.Logging),
		GetAuth:        getAuth(cfg, log),
		RawResponsesCompaction: adaptercodex.RawResponsesCompactionSettings{
			Enabled:                     cfg.MITM.ReorientSummaryInjection,
			ContextWindowTokens:         0,
			FallbackContextWindowTokens: cfg.MITM.ReorientStandardContextWindow,
			MaxTokens:                   cfg.MITM.ReorientInjectMaxTokens,
			ContextWindowFraction:       cfg.MITM.ReorientContextWindowFraction,
			BytesPerToken:               cfg.MITM.ReorientBytesPerToken,
			RecentFraction:              cfg.MITM.ReorientRecentFraction,
		},
		CaptureStore: store,
		Group:        group,
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

// cancelConfigWatcher cancels the config watcher loop without waiting. It is
// nil-safe and used by reload and rebind, where the watcher may be the in-flight
// caller and a blocking drain would deadlock.
func (r *runtimeServices) cancelConfigWatcher() {
	if r == nil || r.configWatcher == nil {
		return
	}
	r.configWatcher.cancelLoop()
}

// shutdown stops the daemon generation through the lifecycle group. It cancels
// the config watcher loop first (non-blocking, so a watcher that is itself the
// caller cannot deadlock), closes the daemon and pprof listeners (which are not
// registries), then runs the group's ordered Quiesce at the shutdown budget.
// Quiesce drains every attached registry and runs the registered hooks
// (keepalives-off, HTTP server shutdowns, SQLite store closes), so the adapter,
// MITM proxies, and the capture store are torn down in phase order with the
// store closed last. Safe on a partially started runtime: only the registries
// and hooks that were attached run.
func (r *runtimeServices) shutdown(parent context.Context) {
	if r == nil {
		return
	}
	r.cancelConfigWatcher()
	if r.listener != nil {
		_ = r.listener.Close()
	}
	if r.pprofListener != nil {
		_ = r.pprofListener.Close()
	}
	if r.group != nil {
		r.group.Quiesce(parent, "shutdown", budgetShutdown)
	}
}
