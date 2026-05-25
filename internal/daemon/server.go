package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	"goodkind.io/clyde/internal/bridge"
	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/contextusage"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/oauthrotation"
	"goodkind.io/clyde/internal/outputstyle"
	codex "goodkind.io/clyde/internal/providers/codex/lifecycle"
	"goodkind.io/clyde/internal/providers/discoveryroots"
	sessionartifacts "goodkind.io/clyde/internal/providers/registry/artifacts"
	"goodkind.io/clyde/internal/providers/sessionname"
	"goodkind.io/clyde/internal/session"
	sessionsettings "goodkind.io/clyde/internal/session/settings"
	"goodkind.io/clyde/internal/slogger"
	itranscript "goodkind.io/clyde/internal/transcript"
)

// Server implements the Clyde gRPC service.
type Server struct {
	clydev1.UnimplementedClydeServiceServer

	log      *slog.Logger
	mu       sync.RWMutex
	sessions map[string]*wrapperSession // keyed by wrapper_id

	watcher        *fsnotify.Watcher
	bridgeWatcher  *bridge.Watcher
	globalSettings map[string]json.RawMessage // last-known global settings.json content

	// scanWake fires the discovery scanner immediately. Buffered so a
	// trigger never blocks the caller.
	scanWake chan discoveryScanSignal

	// subscribers receive registry events as they happen. The mutex
	// guards the map so subscribe and broadcast can run concurrently.
	subMu       sync.Mutex
	subscribers map[chan *clydev1.SubscribeRegistryResponse]registrySubscriberState

	// settingsLocks serialises writes per session name so two callers
	// updating the same session do not stomp on each other. The lock
	// for a session is created lazily and lives for the daemon's
	// lifetime; the cardinality is bounded by the number of sessions.
	settingsLocksMu sync.Mutex
	settingsLocks   map[string]*sync.Mutex

	// bridges maps Claude session UUIDs to the bridge URL exposed by
	// `claude --remote-control`. Populated by the bridge watcher.
	bridgeMu sync.RWMutex
	bridges  map[string]*clydev1.Bridge

	// transcripts hub fans tail lines out to multiple subscribers.
	transcripts   *transcriptHub
	providerStats *providerStatsHub

	remoteMu      sync.Mutex
	remoteWorkers map[string]*remoteWorker
	liveSessions  map[string]*liveRuntimeSession
	liveLeases    map[string]*foregroundLease
	liveWorkers   *livetrack.Registry[LiveMeta]

	contextMu         sync.Mutex
	contextStates     map[string]sessionContextState
	contextRefreshSem chan contextRefreshPermit
	contextUsageCache *contextUsageStateCache
	contextUsageProbe contextUsageProbeFunc

	reloadMu sync.Mutex
	reloadFn func(context.Context) (reloadReport, error)

	mitmAccess mitmAccessor

	oauthRotatorAccess oauthRotatorAccessor

	// launchCredentialDirs tracks scratch dirs into which the daemon planted a
	// rotator-selected account's .credentials.json for a launched provider
	// (CLYDE-448). The launching wrapper removes its own dir when the child
	// exits; this set is the daemon-side backstop so a crashed or abandoned
	// wrapper never leaves planted credentials behind: every tracked dir is
	// removed on daemon Close.
	launchCredentialDirsMu sync.Mutex
	launchCredentialDirs   map[string]bool

	skipRuntimeCleanup atomic.Bool

	autoNameMu sync.RWMutex
	autoName   *autoNameWorker

	RPCs *livetrack.Registry[RPCMeta]
}

// mitmAccessor stores the daemon's optional accessor for the
// daemon-owned MITM proxy. Wrapping the lock and func into a single
// field keeps Server's exhaustruct surface small while still letting
// callers swap the accessor at runtime.
type mitmAccessor struct {
	mu sync.RWMutex
	fn func() *mitm.Proxy
}

// SetMITMProxyAccessor wires a getter the Server uses when callers
// need the daemon-owned MITM proxy. The accessor returns nil when the
// proxy is not running.
func (s *Server) SetMITMProxyAccessor(fn func() *mitm.Proxy) {
	s.mitmAccess.mu.Lock()
	defer s.mitmAccess.mu.Unlock()
	s.mitmAccess.fn = fn
}

func (s *Server) mitmProxy() *mitm.Proxy {
	s.mitmAccess.mu.RLock()
	defer s.mitmAccess.mu.RUnlock()
	if s.mitmAccess.fn == nil {
		return nil
	}
	return s.mitmAccess.fn()
}

// oauthRotatorAccessor stores the daemon's optional accessor for the
// adapter-owned OAuth rotation layer. The adapter builds and owns the rotator;
// the daemon registers a getter here so RPC handlers (a later wave) can reach
// the single live rotator without importing the adapter's internals.
type oauthRotatorAccessor struct {
	mu sync.RWMutex
	fn func() *oauthrotation.Rotator
}

// SetOAuthRotatorAccessor wires a getter the daemon uses when callers need the
// adapter-owned OAuth rotation layer. The accessor returns nil when the
// adapter or its direct-OAuth path is not running.
func (s *Server) SetOAuthRotatorAccessor(fn func() *oauthrotation.Rotator) {
	s.oauthRotatorAccess.mu.Lock()
	defer s.oauthRotatorAccess.mu.Unlock()
	s.oauthRotatorAccess.fn = fn
}

// OAuthRotator returns the daemon's single OAuth rotation layer, or nil when
// the adapter direct-OAuth path is not running. It is the accessor RPC
// handlers use to read accounts and drive logins in a later wave.
func (s *Server) OAuthRotator() *oauthrotation.Rotator {
	s.oauthRotatorAccess.mu.RLock()
	defer s.oauthRotatorAccess.mu.RUnlock()
	if s.oauthRotatorAccess.fn == nil {
		return nil
	}
	return s.oauthRotatorAccess.fn()
}

// wrapperSession holds runtime state for one active claude wrapper process.
type wrapperSession struct {
	wrapperID   string
	sessionName string // empty for bare claude invocations
	sessionID   string
	model       string
	effortLevel string
}

type remoteWorker struct {
	sessionName      string
	sessionID        string
	basedir          string
	incognito        bool
	cmd              *exec.Cmd
	done             chan struct{}
	skipCleanup      atomic.Bool
	livetrackSession *livetrack.Session[LiveMeta]
}

type remoteWorkerTmuxState struct {
	Detected bool
	Status   string
	Reason   string
}

type liveRuntimeSession struct {
	provider         session.ProviderID
	name             string
	id               string
	basedir          string
	model            string
	status           string
	startedAt        time.Time
	lastTurnID       string
	codexRuntime     codex.LiveRuntime
	livetrackSession *livetrack.Session[LiveMeta]
}

type foregroundLease struct {
	token         string
	provider      session.ProviderID
	sessionName   string
	sessionID     string
	basedir       string
	model         string
	status        string
	incognito     bool
	shouldRestore bool
	restoreReason string
	acquiredAt    time.Time
}

func remoteWorkerKey(sessionName, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" {
		return sessionID
	}
	return strings.TrimSpace(sessionName)
}

func removeRemoteWorkerLocked(workers map[string]*remoteWorker, worker *remoteWorker) {
	if worker == nil {
		return
	}
	for key, candidate := range workers {
		if candidate == worker {
			delete(workers, key)
		}
	}
}

var remoteWorkerExecutable = os.Executable

type reloadReport struct {
	BinaryReloaded bool
	NewPID         int
}

type discoveryScanSignal struct {
	Requested   bool
	Correlation correlation.Context
}

type registrySubscriberState bool

type contextRefreshPermit struct {
	Acquired bool
}

type sessionContextState struct {
	Usage      contextusage.Usage
	Loaded     bool
	Status     string
	Refreshing bool
	RetryAfter time.Time
}

func daemonPeerAddr(p *peer.Peer) string {
	if p == nil || p.Addr == nil {
		return ""
	}
	return p.Addr.String()
}

func daemonMetadataKeys(md metadata.MD) []string {
	keys := make([]string, 0, len(md))
	for key := range md {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// SetReloadFunc sets the daemon reload implementation used by ReloadDaemon.
func (s *Server) SetReloadFunc(fn func(context.Context) (reloadReport, error)) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	s.reloadFn = fn
}

func (s *Server) preserveRuntimeDirsOnClose() {
	s.skipRuntimeCleanup.Store(true)
}

// New creates a new daemon Server and starts watching the global settings file.
func New(log *slog.Logger) (*Server, error) {
	log = slogger.WithConcern(log, slogger.ConcernProcessDaemonLifecycle)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create settings watcher: %w", err)
	}

	s := newServerState(log, watcher)

	globalPath := globalSettingsPath()
	if err := s.loadGlobalSettings(context.Background()); err != nil {
		log.LogAttrs(context.Background(), slog.LevelWarn, "global settings load failed on startup",
			slog.String("path", globalPath),
			slog.Any("err", err),
		)
	} else {
		globalModel := globalSettingString(s.globalSettings, "model")
		log.LogAttrs(context.Background(), slog.LevelInfo, "global settings loaded",
			slog.String("path", globalPath),
			slog.String("model", globalModel),
			slog.Int("keys", len(s.globalSettings)),
		)
	}

	if err := watcher.Add(globalPath); err != nil {
		log.LogAttrs(context.Background(), slog.LevelWarn, "failed to watch global settings",
			slog.String("path", globalPath),
			slog.Any("err", err),
		)
	} else {
		log.LogAttrs(context.Background(), slog.LevelInfo, "watching global settings",
			slog.String("path", globalPath),
		)
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WarnContext(context.Background(), "daemon.global_settings.watch_panicked",
					"component", "daemon",
					"panic", r,
				)
			}
		}()
		s.watchGlobalSettings()
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WarnContext(context.Background(), "daemon.discovery_scanner.panicked",
					"component", "daemon",
					"panic", r,
				)
			}
		}()
		s.runDiscoveryScanner()
	}()

	if home, err := os.UserHomeDir(); err == nil {
		sessionsDir := filepath.Join(home, ".claude", "sessions")
		if w, err := bridge.Start(sessionsDir); err == nil {
			s.bridgeWatcher = w
			go func() {
				defer func() {
					if r := recover(); r != nil {
						s.log.WarnContext(context.Background(), "daemon.bridge_watcher.panicked",
							"component", "daemon",
							"panic", r,
						)
					}
				}()
				s.runBridgeWatcher(w)
			}()
			s.log.LogAttrs(context.Background(), slog.LevelInfo, "bridge watcher started",
				slog.String("dir", sessionsDir),
			)
		} else {
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "bridge watcher failed",
				slog.Any("err", err),
			)
		}
	}

	return s, nil
}

func newServerState(log *slog.Logger, watcher *fsnotify.Watcher) *Server {
	return &Server{
		UnimplementedClydeServiceServer: clydev1.UnimplementedClydeServiceServer{},
		log:                             log,
		mu:                              sync.RWMutex{},
		sessions:                        make(map[string]*wrapperSession),
		watcher:                         watcher,
		bridgeWatcher:                   nil,
		globalSettings:                  nil,
		scanWake:                        make(chan discoveryScanSignal, 4),
		subMu:                           sync.Mutex{},
		subscribers:                     make(map[chan *clydev1.SubscribeRegistryResponse]registrySubscriberState),
		settingsLocksMu:                 sync.Mutex{},
		settingsLocks:                   make(map[string]*sync.Mutex),
		bridgeMu:                        sync.RWMutex{},
		bridges:                         make(map[string]*clydev1.Bridge),
		transcripts:                     newTranscriptHub(),
		providerStats:                   newProviderStatsHub(log),
		remoteMu:                        sync.Mutex{},
		remoteWorkers:                   make(map[string]*remoteWorker),
		liveSessions:                    make(map[string]*liveRuntimeSession),
		liveLeases:                      make(map[string]*foregroundLease),
		liveWorkers: livetrack.New[LiveMeta](livetrack.Options[LiveMeta]{
			Component:     "daemon",
			Concern:       "daemon",
			Log:           log,
			PollEvery:     0,
			CloserGrace:   0,
			ParallelClose: false,
			Now:           nil,
		}),
		contextMu:              sync.Mutex{},
		contextStates:          make(map[string]sessionContextState),
		contextRefreshSem:      make(chan contextRefreshPermit, 2),
		contextUsageCache:      newContextUsageStateCache(30 * time.Second),
		contextUsageProbe:      nil,
		reloadMu:               sync.Mutex{},
		reloadFn:               nil,
		mitmAccess:             mitmAccessor{mu: sync.RWMutex{}, fn: nil},
		oauthRotatorAccess:     oauthRotatorAccessor{mu: sync.RWMutex{}, fn: nil},
		launchCredentialDirsMu: sync.Mutex{},
		launchCredentialDirs:   make(map[string]bool),
		skipRuntimeCleanup:     atomic.Bool{},
		autoNameMu:             sync.RWMutex{},
		autoName:               nil,
		RPCs:                   newRPCRegistry(),
	}
}

// runBridgeWatcher consumes bridge events from the watcher and
// translates them into SubscribeRegistryResponse broadcasts.
func (s *Server) runBridgeWatcher(w *bridge.Watcher) {
	// Seed the cache with anything the watcher already saw on its
	// initial scan so ListBridges callers do not race the first event.
	for _, b := range w.Snapshot() {
		s.setBridge(&clydev1.Bridge{
			SessionId:       b.SessionID,
			Pid:             b.PID,
			BridgeSessionId: b.BridgeSessionID,
			Url:             b.URL,
		})
	}
	for ev := range w.Events() {
		switch ev.Kind {
		case bridge.EventOpened:
			s.setBridge(&clydev1.Bridge{
				SessionId:       ev.Bridge.SessionID,
				Pid:             ev.Bridge.PID,
				BridgeSessionId: ev.Bridge.BridgeSessionID,
				Url:             ev.Bridge.URL,
			})
		case bridge.EventClosed:
			s.removeBridge(ev.Bridge.SessionID)
		}
	}
}

// runDiscoveryScanner periodically walks ~/.claude/projects and adopts
// any transcripts whose UUID is not already tracked by clyde. Runs
// once at startup, then on a 5 minute cadence. The scanner wakes
// early when a SIGUSR1 lands or when a TriggerScan RPC arrives so
// clients can refresh the daemon's view immediately. Errors are logged
// but do not stop the loop.
func (s *Server) runDiscoveryScanner() {
	const interval = 5 * time.Minute
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	var pendingWake discoveryScanSignal
	hasPendingWake := false

	for {
		s.runDiscoveryScannerPass(pendingWake, hasPendingWake)
		hasPendingWake = false
		select {
		case <-time.After(interval):
		case <-sig:
			s.log.LogAttrs(context.Background(), slog.LevelDebug, "discovery scan wake from SIGUSR1")
		case wake := <-s.scanWake:
			pendingWake = wake
			hasPendingWake = true
		}
	}
}

func (s *Server) runDiscoveryScannerPass(wake discoveryScanSignal, hasWake bool) {
	scanCtx := context.Background()
	if hasWake {
		scanCtx = daemonDiscoveryScanContext(wake, s.log)
		s.log.LogAttrs(scanCtx, slog.LevelDebug, "discovery scan wake from TriggerScan RPC")
	}
	s.runDiscoveryOnce(scanCtx)
	s.runAutoRenamePass(scanCtx)
}

func daemonDiscoveryScanContext(wake discoveryScanSignal, log *slog.Logger) context.Context {
	if wake.Correlation.TraceID == "" {
		return context.Background()
	}
	return daemonCorrelationContext(context.Background(), log, wake.Correlation.Child())
}

func (s *Server) runDiscoveryOnce(ctx context.Context) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	var results []session.DiscoveryResult
	for _, roots := range discoveryroots.All() {
		for _, path := range roots.Paths(home) {
			if path == "" {
				continue
			}
			if _, statErr := os.Stat(path); statErr != nil {
				continue
			}
			rootResults, scanErr := session.ScanProjects(path)
			if scanErr != nil {
				s.log.LogAttrs(ctx, slog.LevelWarn, "discovery scan failed",
					slog.String("root", path),
					slog.Any("err", scanErr),
				)
				continue
			}
			results = append(results, rootResults...)
		}
	}
	if len(results) == 0 {
		return
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "discovery store init failed",
			slog.Any("err", err),
		)
		return
	}
	changes, err := store.SyncDiscoveryResults(results)
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "discovery sync failed",
			slog.Any("err", err),
		)
		return
	}
	if len(changes) > 0 {
		names := make([]string, 0, len(changes))
		for _, change := range changes {
			if change.Session == nil {
				continue
			}
			names = append(names, change.Session.Name)
			kind := clydev1.SubscribeRegistryResponse_KIND_SESSION_UPDATED
			oldName := ""
			if change.Adopted {
				kind = clydev1.SubscribeRegistryResponse_KIND_SESSION_ADOPTED
			} else if change.OldName != "" && change.Session.Name != change.OldName {
				kind = clydev1.SubscribeRegistryResponse_KIND_SESSION_RENAMED
				oldName = change.OldName
			}
			s.publishSessionSummaryEvent(ctx, kind, store, change.Session, oldName)
		}
		s.log.LogAttrs(ctx, slog.LevelInfo, "discovery synced sessions",
			slog.Int("count", len(changes)),
			slog.Any("names", names),
		)
	}
}

// TriggerScan implements the RPC. The daemon's scanner runs whenever
// the request lands; the response carries any sessions adopted by the
// previous scan tick so the caller has immediate confirmation.
// Subscribers also receive a SESSION_ADOPTED event for each new entry.
func (s *Server) TriggerScan(ctx context.Context, _ *clydev1.TriggerScanRequest) (*clydev1.TriggerScanResponse, error) {
	select {
	case s.scanWake <- discoveryScanSignal{Requested: true, Correlation: correlation.FromContext(ctx)}:
	default:
		// Channel is full; another wake is already pending.
	}
	return &clydev1.TriggerScanResponse{}, nil
}

// SubscribeRegistry streams SubscribeRegistryResponse values to the client until
// the client disconnects. Each subscriber gets its own buffered channel
// so a slow client cannot block the broadcaster. Events that arrive
// while a subscriber's buffer is full are dropped for that one client.
func (s *Server) SubscribeRegistry(_ *clydev1.SubscribeRegistryRequest, stream clydev1.ClydeService_SubscribeRegistryServer) error {
	ch := make(chan *clydev1.SubscribeRegistryResponse, 32)
	streamCtx := stream.Context()
	draining := &atomic.Bool{}
	if s.RPCs != nil {
		rpcSess, err := s.RPCs.Register(streamCtx, "daemon.rpc.subscribe_registry", RPCMeta{
			Direction: "inbound",
			Method:    "SubscribeRegistry",
			PeerActor: func() string {
				peerInfo, _ := peer.FromContext(streamCtx)
				return daemonPeerAddr(peerInfo)
			}(),
			RequestID:  "",
			TraceID:    "",
			LeaseToken: "",
		}, rpcCloser{cancel: func() {}, draining: draining})
		if err == nil {
			defer s.RPCs.Release(streamCtx, rpcSess, "subscribe_registry.done")
		}
	}

	s.subMu.Lock()
	s.subscribers[ch] = true
	s.subMu.Unlock()

	defer func() {
		s.subMu.Lock()
		delete(s.subscribers, ch)
		s.subMu.Unlock()
		close(ch)
	}()

	for {
		if draining.Load() {
			return nil
		}
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				s.log.WarnContext(streamCtx, "daemon.subscribe_registry.send_failed", "err", err)
				return fmt.Errorf("send registry event: %w", err)
			}
		case <-streamCtx.Done():
			return nil
		}
	}
}

// GetProviderStats returns current provider statistics from the daemon cache.
func (s *Server) GetProviderStats(ctx context.Context, _ *clydev1.GetProviderStatsRequest) (*clydev1.GetProviderStatsResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	resp := &clydev1.GetProviderStatsResponse{
		Providers:    s.providerStats.snapshot(),
		LoadedAtUnix: s.providerStats.loadedAtUnix(),
	}
	s.log.DebugContext(ctx, "provider_stats.snapshot_served",
		"component", "daemon",
		"providers", len(resp.GetProviders()),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD),
	)
	return resp, nil
}

// SubscribeProviderStats streams provider statistics updates.
func (s *Server) SubscribeProviderStats(_ *clydev1.SubscribeProviderStatsRequest, stream clydev1.ClydeService_SubscribeProviderStatsServer) error {
	ch := s.providerStats.subscribe()
	defer s.providerStats.unsubscribe(ch)

	ctx := stream.Context()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				s.log.WarnContext(ctx, "daemon.provider_stats.send_failed", "err", err)
				return fmt.Errorf("send provider stats event: %w", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// RenameSession is the daemon-side rename. The daemon owns the rename
// so that no other process can simultaneously mutate the registry
// while the rename is in flight. A SESSION_RENAMED event broadcasts
// the change to every subscriber.
func (s *Server) RenameSession(ctx context.Context, req *clydev1.RenameSessionRequest) (*clydev1.RenameSessionResponse, error) {
	if req.GetOldName() == "" || req.GetNewName() == "" {
		return nil, status.Error(codes.InvalidArgument, "old_name and new_name are required")
	}
	if err := s.renameSessionInternal(ctx, req.GetOldName(), req.GetNewName(), renameAttribution{
		state:      session.AutoNameStateUserLocked,
		source:     session.AutoNameSourceUser,
		sourceHash: "",
	}); err != nil {
		return nil, err
	}
	return &clydev1.RenameSessionResponse{}, nil
}

// ApplyAutoRename is the daemon entry point the auto-name worker uses.
func (s *Server) ApplyAutoRename(ctx context.Context, oldName, newName, sourceHash string) error {
	if oldName == "" || newName == "" {
		return status.Error(codes.InvalidArgument, "old_name and new_name are required")
	}
	return s.renameSessionInternal(ctx, oldName, newName, renameAttribution{
		state:      session.AutoNameStateApplied,
		source:     session.AutoNameSourceTranscript,
		sourceHash: sourceHash,
	})
}

// HasLiveLease reports whether any wrapper currently holds a live-runtime
// record or foreground lease against the named session.
func (s *Server) HasLiveLease(name string) bool {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	for _, live := range s.liveSessions {
		if live != nil && live.name == name {
			return true
		}
	}
	for _, lease := range s.liveLeases {
		if lease != nil && lease.sessionName == name {
			return true
		}
	}
	return false
}

func (s *Server) sessionHasLiveLease(name string) bool {
	return s.HasLiveLease(name)
}

type renameAttribution struct {
	state      session.AutoNameState
	source     session.AutoNameSource
	sourceHash string
}

func (s *Server) renameSessionInternal(ctx context.Context, oldName, newName string, attribution renameAttribution) error {
	start := daemonNow()
	if s.sessionHasLiveLease(oldName) {
		return status.Errorf(codes.FailedPrecondition, "session %q is in use; close the wrapper before renaming", oldName)
	}

	store, err := session.NewGlobalFileStore()
	if err != nil {
		return status.Errorf(codes.Internal, "store init: %v", err)
	}
	existing, err := store.Get(oldName)
	if err != nil {
		return status.Errorf(codes.Internal, "load session before rename: %v", err)
	}
	if err := s.writeProviderSessionName(ctx, existing, newName); err != nil {
		return err
	}
	if err := store.Rename(oldName, newName); err != nil {
		return status.Errorf(codes.Internal, "rename failed: %v", err)
	}
	renamed, getErr := store.Get(newName)
	if getErr == nil && renamed != nil {
		renamed.Metadata.AutoNameState = attribution.state
		renamed.Metadata.AutoNameSource = attribution.source
		renamed.Metadata.AutoNameSourceHash = attribution.sourceHash
		renamed.Metadata.LastAutoNameAt = daemonNow()
		if updateErr := store.Update(renamed); updateErr != nil {
			s.log.WarnContext(ctx, "daemon.rename.metadata_update_failed",
				"component", "daemon",
				"subcomponent", "session",
				"session", newName,
				"err", updateErr,
			)
		}
	}
	s.renameContextState(oldName, newName)
	s.publishSessionSummaryEvent(ctx, clydev1.SubscribeRegistryResponse_KIND_SESSION_RENAMED, store, renamed, oldName)
	sessionID := ""
	if renamed != nil {
		sessionID = renamed.Metadata.ProviderSessionID()
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "daemon.rename.applied",
		slog.String("component", "daemon"),
		slog.String("subcomponent", "session"),
		slog.String("prior_name", oldName),
		slog.String("new_name", newName),
		slog.String("session_id", sessionID),
		slog.String("trace_id", string(correlation.FromContext(ctx).TraceID)),
		slog.String("source", attribution.source.String()),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
	return nil
}

func (s *Server) writeProviderSessionName(ctx context.Context, sess *session.Session, newName string) error {
	if sess == nil {
		return status.Errorf(codes.NotFound, "session not found")
	}
	if err := sessionname.Rename(ctx, sess, newName); err != nil {
		return status.Errorf(codes.Internal, "provider rename failed: %v", err)
	}
	return nil
}

// DeleteSession is the daemon-side delete. It removes registry metadata,
// provider transcripts, agent logs, and per-session output style artifacts,
// then broadcasts SESSION_DELETED so dashboards prune the row immediately.
func (s *Server) DeleteSession(ctx context.Context, req *clydev1.DeleteSessionRequest) (*clydev1.DeleteSessionResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetName())
	}
	if _, err := sessionartifacts.Delete(ctx, session.DeleteArtifactsRequest{
		Session:   sess,
		ClydeRoot: daemonProjectClydeRootForSession(sess),
	}); err != nil {
		s.log.WarnContext(ctx, "daemon.session_delete.provider_artifacts_failed",
			"component", "daemon",
			"subcomponent", "sessions",
			"session", sess.Name,
			"provider", sess.ProviderID(),
			"peer_addr", daemonPeerAddr(peerInfo),
			"err", err,
		)
	}
	if sess.Metadata.HasCustomOutputStyle {
		if err := outputstyle.DeleteCustomStyleFile(config.GlobalOutputStyleRoot(), sess.StorageKey()); err != nil {
			s.log.WarnContext(ctx, "daemon.session_delete.output_style_failed",
				"component", "daemon",
				"subcomponent", "sessions",
				"session", sess.Name,
				"peer_addr", daemonPeerAddr(peerInfo),
				"err", err)
		}
	}
	if err := store.Delete(req.GetName()); err != nil {
		return nil, status.Errorf(codes.Internal, "delete failed: %v", err)
	}
	s.deleteContextState(req.GetName())
	s.publishEvent(&clydev1.SubscribeRegistryResponse{
		Kind:        clydev1.SubscribeRegistryResponse_KIND_SESSION_DELETED,
		SessionName: req.GetName(),
	})
	s.log.LogAttrs(ctx, slog.LevelInfo, "session deleted via RPC",
		slog.String("name", req.GetName()),
		slog.String("peer_addr", daemonPeerAddr(peerInfo)),
		slog.Any("metadata_keys", daemonMetadataKeys(incomingMD)),
	)
	return &clydev1.DeleteSessionResponse{}, nil
}

// UpdateSessionMetadata is the daemon-owned write path for session metadata
// fields that the TUI can edit in-place (for now: workspace root).
func (s *Server) UpdateSessionMetadata(ctx context.Context, req *clydev1.UpdateSessionMetadataRequest) (*clydev1.UpdateSessionMetadataResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Get(req.GetName())
	if err != nil || sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetName())
	}
	sess.Metadata.WorkspaceRoot = strings.TrimSpace(req.GetWorkspaceRoot())
	if err := store.Update(sess); err != nil {
		return nil, status.Errorf(codes.Internal, "update session metadata: %v", err)
	}
	s.publishSessionSummaryEvent(ctx, clydev1.SubscribeRegistryResponse_KIND_SESSION_UPDATED, store, sess, "")
	s.log.LogAttrs(ctx, slog.LevelInfo, "session metadata updated via RPC",
		slog.String("session", sess.Name),
		slog.String("workspace_root", sess.Metadata.WorkspaceRoot),
	)
	return &clydev1.UpdateSessionMetadataResponse{}, nil
}

func daemonProjectClydeRootForSession(sess *session.Session) string {
	root := sess.Metadata.WorkspaceRoot
	if root == "" {
		root, _ = config.FindProjectRoot()
	}
	return filepath.Join(root, config.ClydeDir)
}

// publishEvent fans an event out to every active subscriber. Slow
// subscribers whose buffer is full silently drop the event to keep the
// broadcaster non-blocking.
func (s *Server) publishEvent(ev *clydev1.SubscribeRegistryResponse) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *Server) publishSessionSummaryEvent(ctx context.Context, kind clydev1.SubscribeRegistryResponse_Kind, store *session.FileStore, sess *session.Session, oldName string) {
	if sess == nil {
		return
	}
	ev := &clydev1.SubscribeRegistryResponse{
		Kind:        kind,
		SessionName: sess.Name,
		SessionId:   sess.Metadata.ProviderSessionID(),
		OldName:     oldName,
	}
	if store != nil {
		ev.SessionSummary = s.sessionSummary(ctx, store, sess)
	}
	s.publishEvent(ev)
}

func (s *Server) publishGlobalSettingsEvent(globalRemoteControl bool) {
	s.publishEvent(&clydev1.SubscribeRegistryResponse{
		Kind:                clydev1.SubscribeRegistryResponse_KIND_GLOBAL_SETTINGS_UPDATED,
		GlobalRemoteControl: globalRemoteControl,
	})
}

// Close shuts down the watcher and cleans up all active session runtime dirs.
func (s *Server) Close() {
	bridge.Close(s.bridgeWatcher)
	if s.watcher != nil {
		_ = s.watcher.Close()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sess := range s.sessions {
		if !s.skipRuntimeCleanup.Load() {
			_ = os.RemoveAll(config.SessionRuntimeDir(sess.wrapperID))
		}
	}
	cleanedLaunchCredentialDirs := s.removeLaunchCredentialDirs()
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "daemon closed",
		slog.Int("cleaned_sessions", len(s.sessions)),
		slog.Int("cleaned_launch_credential_dirs", cleanedLaunchCredentialDirs),
		slog.Bool("preserved_runtime_dirs", s.skipRuntimeCleanup.Load()),
	)
}

// AcquireSession writes a per-session settings.json (global settings with
// model overridden) and returns the path along with the real claude binary.
func (s *Server) AcquireSession(ctx context.Context, req *clydev1.AcquireSessionRequest) (*clydev1.AcquireSessionResponse, error) {
	if req.GetWrapperId() == "" || req.GetWrapperId() == "__probe__" {
		return nil, status.Error(codes.InvalidArgument, "wrapper_id is required")
	}

	// Check if this session already has a settings file on disk (re-acquire
	// after daemon restart). Preserve its current model/effort rather than
	// resetting to global defaults.
	existingModel, existingEffort := s.readSessionSettings(req.GetWrapperId())

	sessionName := strings.TrimSpace(req.GetSessionName())
	sessionID := strings.TrimSpace(req.GetSessionId())
	var model, effortLevel string
	if existingModel != "" || existingEffort != "" {
		// Re-registering after daemon restart. Preserve what claude has.
		model = existingModel
		effortLevel = existingEffort
		s.log.LogAttrs(ctx, slog.LevelInfo, "re-acquired session with preserved settings",
			slog.String("wrapper_id", req.GetWrapperId()),
			slog.String("model", model),
			slog.String("cursor_normalized_model", adaptercursor.NormalizeModelAlias(model)),
			slog.String("effort", effortLevel),
		)
	} else {
		// Fresh session. Resolve from clyde session settings and global config.
		resolvedSession, resolvedModel, resolvedEffort := s.resolveSessionSettings(ctx, sessionName, sessionID)
		if resolvedSession != nil {
			sessionName = firstNonEmpty(resolvedSession.Name, sessionName)
			sessionID = firstNonEmpty(resolvedSession.Metadata.ProviderSessionID(), sessionID)
		}
		model = resolvedModel
		effortLevel = resolvedEffort
	}

	settingsFile, err := s.writeSettingsJSON(ctx, req.GetWrapperId(), model, effortLevel)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to write settings: %v", err)
	}

	sess := &wrapperSession{
		wrapperID:   req.GetWrapperId(),
		sessionName: sessionName,
		sessionID:   sessionID,
		model:       model,
		effortLevel: effortLevel,
	}

	s.mu.Lock()
	s.sessions[req.GetWrapperId()] = sess
	s.mu.Unlock()

	realClaude, err := findRealClaude()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to find real claude binary: %v", err)
	}

	s.log.LogAttrs(ctx, slog.LevelInfo, "session acquired",
		slog.String("wrapper_id", req.GetWrapperId()),
		slog.String("session", sessionName),
		slog.String("session_id", sessionID),
		slog.String("model", model),
		slog.String("cursor_normalized_model", adaptercursor.NormalizeModelAlias(model)),
		slog.String("settings_file", settingsFile),
		slog.String("claude_bin", realClaude),
		slog.Int("active_sessions", len(s.sessions)),
	)

	return &clydev1.AcquireSessionResponse{
		RealClaude:   realClaude,
		Model:        model,
		SettingsFile: settingsFile,
	}, nil
}

// ReleaseSession removes the per-session runtime dir after claude exits.
// When the last session is released, the idle timer starts.
func (s *Server) ReleaseSession(ctx context.Context, req *clydev1.ReleaseSessionRequest) (*clydev1.ReleaseSessionResponse, error) {
	s.mu.Lock()
	sess, ok := s.sessions[req.GetWrapperId()]
	if ok {
		delete(s.sessions, req.GetWrapperId())
	}
	remaining := len(s.sessions)
	s.mu.Unlock()

	if ok {
		_ = os.RemoveAll(config.SessionRuntimeDir(sess.wrapperID))
		s.log.LogAttrs(ctx, slog.LevelInfo, "session released",
			slog.String("wrapper_id", req.GetWrapperId()),
			slog.String("session", sess.sessionName),
			slog.String("model", sess.model),
			slog.Int("active_sessions", remaining),
		)
	} else {
		s.log.LogAttrs(ctx, slog.LevelWarn, "release for unknown session",
			slog.String("wrapper_id", req.GetWrapperId()),
		)
	}

	return &clydev1.ReleaseSessionResponse{}, nil
}

// hookEventPayload is the JSON structure sent via HookEvent RPC.
type hookEventPayload struct {
	Type          string   `json:"type"`
	SessionName   string   `json:"session_name,omitempty"`
	WorkspaceRoot string   `json:"workspace_root,omitempty"`
	Messages      []string `json:"messages,omitempty"` // pre-extracted recent messages
}

// HookEvent processes a Claude Code hook event forwarded from a wrapper process.
func (s *Server) HookEvent(ctx context.Context, req *clydev1.HookEventRequest) (*clydev1.HookEventResponse, error) {
	_, _ = peer.FromContext(ctx)

	var payload hookEventPayload
	if err := json.Unmarshal(req.GetRawJson(), &payload); err != nil {
		s.log.WarnContext(ctx, "daemon.hook_event.decode_failed",
			"component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("decode hook event payload: %w", err)
	}

	switch payload.Type {
	case "update_context":
		// Stubbed. The previous implementation shelled out to
		// `claude -p --model sonnet` which recursed through the
		// SessionStart hook chain (claude -> clyde hook -> daemon
		// -> claude -p ...) and fanned out a process tree until the
		// host ran out of file descriptors. The whole subsystem is
		// disabled until it is rebuilt against the in-process
		// adapter. See config.LabelerConfig for the future wiring
		// point.
		// Intentionally silent to avoid noisy steady-state daemon logs.
	}

	return &clydev1.HookEventResponse{ExitCode: 0}, nil
}

// adapterScratchDir returns the cwd used for OpenAI adapter spawned
// claude -p calls. The path is created lazily and cached.
var (
	adapterScratchOnce sync.Once
	adapterScratchPath string
)

func adapterScratchDir() string {
	adapterScratchOnce.Do(func() {
		base, err := os.UserCacheDir()
		if err != nil {
			return
		}
		dir := filepath.Join(base, "clyde", "adapter-scratch")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		adapterScratchPath = dir
	})
	return adapterScratchPath
}

// writeSettingsJSON writes a per-session settings.json to the runtime dir,
// merging global settings with the per-session model override.
func (s *Server) writeSettingsJSON(ctx context.Context, wrapperID, model, effortLevel string) (string, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)

	s.mu.RLock()
	globalCopy := make(map[string]json.RawMessage, len(s.globalSettings))
	maps.Copy(globalCopy, s.globalSettings)
	globalModel := globalSettingString(s.globalSettings, "model")
	s.mu.RUnlock()

	model = adaptercursor.NormalizeSessionSettingsModel(model)
	if model != "" {
		if encoded, err := json.Marshal(model); err == nil {
			globalCopy["model"] = encoded
		}
	}
	if effortLevel != "" {
		if encoded, err := json.Marshal(effortLevel); err == nil {
			globalCopy["effortLevel"] = encoded
		}
	}

	effectiveModel := globalSettingString(globalCopy, "model")
	s.log.LogAttrs(ctx, slog.LevelDebug, "writing per-session settings",
		slog.String("wrapper_id", wrapperID),
		slog.String("global_model", globalModel),
		slog.String("session_model", model),
		slog.String("cursor_normalized_session_model", adaptercursor.NormalizeModelAlias(model)),
		slog.String("session_effort", effortLevel),
		slog.String("effective_model", effectiveModel),
		slog.Int("settings_keys", len(globalCopy)),
	)

	data, err := json.MarshalIndent(globalCopy, "", "  ")
	if err != nil {
		s.log.WarnContext(ctx, "daemon.settings.marshal_failed",
			"component", "daemon",
			"wrapper_id", wrapperID,
			"err", err,
		)
		return "", fmt.Errorf("failed to marshal settings: %w", err)
	}

	sessionDir := config.SessionRuntimeDir(wrapperID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		s.log.WarnContext(ctx, "daemon.settings.session_dir_create_failed",
			"component", "daemon",
			"wrapper_id", wrapperID,
			"path", sessionDir,
			"err", err,
		)
		return "", fmt.Errorf("failed to create session dir: %w", err)
	}

	settingsPath := filepath.Join(sessionDir, "settings.json")
	if err := os.WriteFile(settingsPath, data, 0o600); err != nil {
		s.log.WarnContext(ctx, "daemon.settings.write_failed",
			"component", "daemon",
			"wrapper_id", wrapperID,
			"path", settingsPath,
			"err", err,
		)
		return "", fmt.Errorf("failed to write settings.json: %w", err)
	}

	return settingsPath, nil
}

// syncAllSessions rewrites settings.json for all active sessions when the
// global settings file changes. Each session's current model is preserved
// so that /model changes in one session don't leak to others.
func (s *Server) syncAllSessions(ctx context.Context) {
	s.mu.RLock()
	sessions := make([]*wrapperSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.RUnlock()

	for _, sess := range sessions {
		currentModel, currentEffort := s.readSessionSettings(sess.wrapperID)
		if currentModel != "" {
			sess.model = currentModel
		}
		if currentEffort != "" {
			sess.effortLevel = currentEffort
		}
		s.log.LogAttrs(ctx, slog.LevelDebug, "syncing session",
			slog.String("wrapper_id", sess.wrapperID),
			slog.String("session", sess.sessionName),
			slog.String("preserved_model", sess.model),
			slog.String("preserved_effort", sess.effortLevel),
		)
		if _, err := s.writeSettingsJSON(ctx, sess.wrapperID, sess.model, sess.effortLevel); err != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "failed to sync settings",
				slog.String("wrapper_id", sess.wrapperID),
				slog.Any("err", err),
			)
		}
	}

	s.log.LogAttrs(ctx, slog.LevelInfo, "global settings synced to all sessions",
		slog.Int("active_sessions", len(sessions)),
	)
}

// sessionSettingsFile is the on-disk settings.json shape read by
// readSessionSettings for global-settings sync.
type sessionSettingsFile struct {
	Model       string `json:"model"`
	EffortLevel string `json:"effortLevel"`
}

// readSessionSettings reads model and effortLevel from a session's current settings.json.
// Returns "" for either value if the file doesn't exist or lacks the field.
func (s *Server) readSessionSettings(wrapperID string) (model, effortLevel string) {
	path := filepath.Join(config.SessionRuntimeDir(wrapperID), "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var settings sessionSettingsFile
	if err := json.Unmarshal(data, &settings); err != nil {
		return "", ""
	}
	model = adaptercursor.NormalizeSessionSettingsModel(settings.Model)
	effortLevel = settings.EffortLevel
	return model, effortLevel
}

// watchGlobalSettings runs in a goroutine, syncing global settings changes
// to all active sessions.
func (s *Server) watchGlobalSettings() {
	for {
		select {
		case event, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				s.log.LogAttrs(context.Background(), slog.LevelDebug, "global settings file changed",
					slog.String("event", event.Op.String()),
				)
				if err := s.reloadGlobalSettings(context.Background()); err != nil {
					s.log.LogAttrs(context.Background(), slog.LevelWarn, "failed to reload global settings",
						slog.Any("err", err),
					)
					continue
				}
			}

		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			s.log.LogAttrs(context.Background(), slog.LevelWarn, "settings watcher error",
				slog.Any("err", err),
			)
		}
	}
}

func (s *Server) reloadGlobalSettings(ctx context.Context) error {
	if err := s.loadGlobalSettings(ctx); err != nil {
		return err
	}
	s.syncAllSessions(ctx)
	s.log.LogAttrs(ctx, slog.LevelInfo, "daemon global settings reloaded",
		slog.Int("active_sessions", s.activeSessionCount()),
	)
	return nil
}

func (s *Server) activeSessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// ReloadDaemon starts a replacement daemon binary and then lets the
// current process drain. Existing accepted gRPC streams stay attached
// to this process until they finish; new clients connect to the
// replacement once it has rebound the runtime socket.
func (s *Server) ReloadDaemon(ctx context.Context, req *clydev1.ReloadDaemonRequest) (*clydev1.ReloadDaemonResponse, error) {
	start := daemonNow()
	s.reloadMu.Lock()
	fn := s.reloadFn
	s.reloadMu.Unlock()

	if fn == nil {
		return nil, status.Error(codes.FailedPrecondition, "daemon reload is not available")
	}
	report, err := fn(ctx)
	if err != nil {
		if errors.Is(err, errReloadBeforeProcessLock) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "reload binary: %v", err)
	}
	active := s.activeSessionCount()
	s.log.LogAttrs(ctx, slog.LevelInfo, "daemon reload completed",
		slog.Int("active_sessions", active),
		slog.Bool("binary_reloaded", report.BinaryReloaded),
		slog.Int("new_pid", report.NewPID),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()),
	)
	return &clydev1.ReloadDaemonResponse{
		ActiveSessions: compactInt32(active),
		BinaryReloaded: report.BinaryReloaded,
		NewPid:         int64(report.NewPID),
	}, nil
}

// loadGlobalSettings reads ~/.claude/settings.json into memory.
func (s *Server) loadGlobalSettings(ctx context.Context) error {
	path := globalSettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.log.LogAttrs(ctx, slog.LevelDebug, "global settings file not found, using empty",
				slog.String("path", path),
			)
			s.mu.Lock()
			s.globalSettings = make(map[string]json.RawMessage)
			s.mu.Unlock()
			return nil
		}
		s.log.WarnContext(ctx, "daemon.global_settings.read_failed",
			"component", "daemon",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("failed to read global settings: %w", err)
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(data, &settings); err != nil {
		s.log.WarnContext(ctx, "daemon.global_settings.parse_failed",
			"component", "daemon",
			"path", path,
			"err", err,
		)
		return fmt.Errorf("failed to parse global settings: %w", err)
	}

	model := globalSettingString(settings, "model")
	s.log.LogAttrs(ctx, slog.LevelDebug, "global settings reloaded",
		slog.String("model", model),
		slog.Int("keys", len(settings)),
	)

	s.mu.Lock()
	s.globalSettings = settings
	s.mu.Unlock()

	return nil
}

// resolveSessionSettings loads per-session model and effortLevel from the
// stored clyde session, preferring stable provider identity when available and
// falling back to global settings for any field not set at the session level.
func (s *Server) resolveSessionSettings(ctx context.Context, sessionName, sessionID string) (*session.Session, string, string) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	s.mu.RLock()
	globalModel := globalSettingString(s.globalSettings, "model")
	globalEffort := globalSettingString(s.globalSettings, "effortLevel")
	s.mu.RUnlock()

	model := globalModel
	model = adaptercursor.NormalizeSessionSettingsModel(model)
	effortLevel := globalEffort

	if strings.TrimSpace(sessionName) == "" && strings.TrimSpace(sessionID) == "" {
		s.log.LogAttrs(ctx, slog.LevelDebug, "no session name, using global settings",
			slog.String("model", model),
			slog.String("cursor_normalized_model", adaptercursor.NormalizeModelAlias(model)),
			slog.String("effort", effortLevel),
		)
		return nil, model, effortLevel
	}

	store, err := session.NewGlobalFileStore()
	if err != nil {
		s.log.WarnContext(ctx, "daemon.session_settings.store_init_failed",
			"component", "daemon",
			"session", sessionName,
			"session_id", sessionID,
			"err", err,
		)
		return nil, model, effortLevel
	}
	resolvedSession, err := resolveStoredSession(store, sessionName, sessionID)
	if err != nil {
		s.log.WarnContext(ctx, "daemon.session_settings.resolve_failed",
			"component", "daemon",
			"session", sessionName,
			"session_id", sessionID,
			"err", err,
		)
		return nil, model, effortLevel
	}
	if resolvedSession == nil {
		s.log.LogAttrs(ctx, slog.LevelDebug, "no clyde session row, using global",
			slog.String("session", sessionName),
			slog.String("session_id", sessionID),
			slog.String("model", model),
			slog.String("cursor_normalized_model", adaptercursor.NormalizeModelAlias(model)),
			slog.String("effort", effortLevel),
		)
		return nil, model, effortLevel
	}
	sessSettings, err := sessionsettings.Load(store, resolvedSession)
	switch {
	case err == nil && sessSettings != nil:
		if sessSettings.Model != "" {
			model = adaptercursor.NormalizeSessionSettingsModel(sessSettings.Model)
		}
		if sessSettings.EffortLevel != "" {
			effortLevel = sessSettings.EffortLevel
		}
		s.log.LogAttrs(ctx, slog.LevelDebug, "resolved session settings",
			slog.String("session", firstNonEmpty(resolvedSession.Name, sessionName)),
			slog.String("session_id", firstNonEmpty(resolvedSession.Metadata.ProviderSessionID(), sessionID)),
			slog.String("model", model),
			slog.String("cursor_normalized_model", adaptercursor.NormalizeModelAlias(model)),
			slog.String("effort", effortLevel),
		)
	case err != nil:
		s.log.WarnContext(ctx, "daemon.session_settings.load_failed",
			"component", "daemon",
			"session", resolvedSession.Name,
			"session_id", firstNonEmpty(resolvedSession.Metadata.ProviderSessionID(), sessionID),
			"err", err,
		)
	default:
		s.log.LogAttrs(ctx, slog.LevelDebug, "no clyde session settings, using global",
			slog.String("session", resolvedSession.Name),
			slog.String("session_id", firstNonEmpty(resolvedSession.Metadata.ProviderSessionID(), sessionID)),
			slog.String("model", model),
			slog.String("cursor_normalized_model", adaptercursor.NormalizeModelAlias(model)),
			slog.String("effort", effortLevel),
		)
	}

	return resolvedSession, model, effortLevel
}

func globalSettingString(settings map[string]json.RawMessage, key string) string {
	if len(settings) == 0 {
		return ""
	}
	raw, ok := settings[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return ""
	}
	return out
}

func resolveStoredSession(store *session.FileStore, sessionName, sessionID string) (*session.Session, error) {
	if store == nil {
		return nil, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" {
		sess, err := store.Resolve(sessionID)
		if err != nil {
			slog.Warn("daemon.resolve_stored_session.id_failed", "session_id", sessionID, "err", err)
			return nil, fmt.Errorf("resolve session id %q: %w", sessionID, err)
		}
		if sess != nil {
			return sess, nil
		}
	}
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return nil, nil
	}
	sess, err := store.Resolve(sessionName)
	if err != nil {
		slog.Warn("daemon.resolve_stored_session.name_failed", "session", sessionName, "err", err)
		return nil, fmt.Errorf("resolve session name %q: %w", sessionName, err)
	}
	return sess, nil
}

func resolvedSessionName(store *session.FileStore, sessionName, sessionID string) string {
	sess, err := resolveStoredSession(store, sessionName, sessionID)
	if err == nil && sess != nil && strings.TrimSpace(sess.Name) != "" {
		return sess.Name
	}
	return strings.TrimSpace(sessionName)
}

func globalSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// ListActiveSessions returns all currently acquired sessions.
func (s *Server) ListActiveSessions(_ context.Context, _ *clydev1.ListActiveSessionsRequest) (*clydev1.ListActiveSessionsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, _ := session.NewGlobalFileStore()

	var active []*clydev1.ActiveSession
	for wid, sess := range s.sessions {
		if sess == nil {
			continue
		}
		active = append(active, &clydev1.ActiveSession{
			SessionName: resolvedSessionName(store, sess.sessionName, sess.sessionID),
			WrapperId:   wid,
		})
	}

	return &clydev1.ListActiveSessionsResponse{Sessions: active}, nil
}

func (s *Server) sessionIsActive(sessionName string) bool {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return false
	}
	targetSessionID := ""
	store, err := session.NewGlobalFileStore()
	if err == nil {
		if sess, resolveErr := resolveStoredSession(store, sessionName, ""); resolveErr == nil && sess != nil {
			targetSessionID = sess.Metadata.ProviderSessionID()
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess == nil {
			continue
		}
		if targetSessionID != "" && strings.TrimSpace(sess.sessionID) == targetSessionID {
			return true
		}
		if sess.sessionName == sessionName {
			return true
		}
	}
	return false
}

// ListSessions returns all sessions known to the daemon registry.
func (s *Server) ListSessions(ctx context.Context, _ *clydev1.ListSessionsRequest) (*clydev1.ListSessionsResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	started := daemonNow()
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sessions, err := store.List()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list sessions: %v", err)
	}
	out := make([]*clydev1.SessionSummary, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, s.sessionSummary(ctx, store, sess))
	}
	globalRC := false
	if cfg, err := config.LoadGlobalOrDefault(); err == nil {
		globalRC = cfg.Defaults.RemoteControl
	}
	s.log.DebugContext(ctx, "daemon.sessions.list.completed",
		"component", "daemon",
		"subcomponent", "sessions",
		"duration_ms", time.Since(started).Milliseconds(),
		"sessions_total", len(out),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD))
	return &clydev1.ListSessionsResponse{Sessions: out, GlobalRemoteControl: globalRC}, nil
}

// GetSessionDetail returns detailed metadata and transcript-derived state for one session.
func (s *Server) GetSessionDetail(ctx context.Context, req *clydev1.GetSessionDetailRequest) (*clydev1.GetSessionDetailResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name is required")
	}
	started := daemonNow()
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	detail := s.sessionDetail(ctx, store, sess)
	s.log.DebugContext(ctx, "daemon.session_detail.completed",
		"component", "daemon",
		"subcomponent", "sessions",
		"duration_ms", time.Since(started).Milliseconds(),
		"session", sess.Name,
		"messages_total", detail.GetTotalMessages(),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD))
	return detail, nil
}

// GetSessionExportStats returns transcript export statistics for one session.
func (s *Server) GetSessionExportStats(ctx context.Context, req *clydev1.GetSessionExportStatsRequest) (*clydev1.GetSessionExportStatsResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name is required")
	}
	started := daemonNow()
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	if !sess.SessionProviderCapabilities().TranscriptExport {
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support transcript export", sess.ProviderID())
	}
	stats := inspectExportStatsForSession(sess)
	resp := &clydev1.GetSessionExportStatsResponse{
		SessionName:           sess.Name,
		VisibleTokensEstimate: compactInt32(stats.VisibleTokensEstimate),
		VisibleMessages:       compactInt32(stats.VisibleMessages),
		UserMessages:          compactInt32(stats.UserMessages),
		AssistantMessages:     compactInt32(stats.AssistantMessages),
		ToolResultMessages:    compactInt32(stats.ToolResultMessages),
		ToolCalls:             compactInt32(stats.ToolCalls),
		SystemPrompts:         compactInt32(stats.SystemPrompts),
		Compactions:           compactInt32(stats.Compactions),
		TranscriptSizeBytes:   stats.TranscriptSizeBytes,
	}
	s.log.DebugContext(ctx, "daemon.session_export_stats.completed",
		"component", "daemon",
		"subcomponent", "sessions",
		"duration_ms", time.Since(started).Milliseconds(),
		"session", sess.Name,
		"visible_messages", resp.GetVisibleMessages(),
		"compactions", resp.GetCompactions(),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD))
	return resp, nil
}

// ExportSession returns a serialized transcript export for one session.
func (s *Server) ExportSession(ctx context.Context, req *clydev1.ExportSessionRequest) (*clydev1.ExportSessionResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name is required")
	}
	started := daemonNow()
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	if !sess.SessionProviderCapabilities().TranscriptExport {
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support transcript export", sess.ProviderID())
	}
	body, err := buildSessionExport(sess, req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "build export: %v", err)
	}
	s.log.InfoContext(ctx, "daemon.session_export.completed",
		"component", "daemon",
		"subcomponent", "sessions",
		"duration_ms", time.Since(started).Milliseconds(),
		"session", sess.Name,
		"format", req.GetFormat().String(),
		"history_start", req.GetHistoryStart(),
		"bytes", len(body),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD))
	return &clydev1.ExportSessionResponse{Body: body}, nil
}

func (s *Server) contextStateForSession(ctx context.Context, sess *session.Session) sessionContextState {
	_ = ctx
	if sess == nil || !sess.SessionProviderCapabilities().ContextUsageInspect || sess.Name == "" || sess.Metadata.ProviderSessionID() == "" || strings.TrimSpace(sess.Metadata.ProviderTranscriptPath()) == "" {
		return emptySessionContextState()
	}

	state, ok := s.contextUsageStateCache().Get(sess)
	if !ok {
		return emptySessionContextState()
	}
	return state
}

// serverDetailLazyProbeWaitBudgetForTest bounds how long
// GetSessionDetail blocks waiting for a fresh context-usage probe to
// finish before returning a placeholder with Status="probing". The
// probe itself keeps running on a daemon-rooted context so the cache
// fills regardless of whether the RPC client stays connected. Future
// detail calls hit the warm cache. Exposed as a var purely so tests
// can shrink the budget; production code does not mutate it.
var serverDetailLazyProbeWaitBudgetForTest = 4 * time.Second

// detailLazyProbeMaxDuration bounds how long a single detail-triggered
// probe is allowed to run on the daemon background context. If the
// probe exceeds this it is cancelled and recorded as a failure so the
// per-session cooldown gates further attempts.
const detailLazyProbeMaxDuration = 90 * time.Second

// lazyContextStateForDetail returns the cached context state for sess,
// kicking off at most one probe per cache-key when the cache is cold.
// The probe runs on the daemon background context so it survives RPC
// client disconnect, but the RPC waits at most detailLazyProbeWaitBudget
// before returning a placeholder. The S0 hot-loop is prevented by:
//   - cache.Refresh in-flight dedup (one probe per session at a time)
//   - the existing contextRefreshSem semaphore (daemon-wide cap)
//   - per-session cooldown after a failed probe
//   - size-keyed cache (no probe until the transcript grows)
//   - bounded probe duration
//   - probe_started/probe_completed log markers for regression detection
func (s *Server) lazyContextStateForDetail(ctx context.Context, sess *session.Session) sessionContextState {
	if sess == nil || !sess.SessionProviderCapabilities().ContextUsageInspect {
		return emptySessionContextState()
	}
	if sess.Name == "" || sess.Metadata.ProviderSessionID() == "" || strings.TrimSpace(sess.Metadata.ProviderTranscriptPath()) == "" {
		return emptySessionContextState()
	}
	if cached, ok := s.contextUsageStateCache().Get(sess); ok {
		return cached
	}

	probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(ctx), detailLazyProbeMaxDuration)
	resultCh := make(chan sessionContextState, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(probeCtx, "daemon.session_detail.context_probe.panic",
					"session", sess,
					"err", fmt.Errorf("panic: %v", r),
				)
				failed := emptySessionContextState()
				failed.Status = "probe_failed"
				resultCh <- failed
			}
		}()
		defer probeCancel()
		state, err := s.refreshContextUsageState(probeCtx, sess)
		if err != nil && state.Status == "" {
			state.Status = "probe_failed"
		}
		resultCh <- state
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, serverDetailLazyProbeWaitBudgetForTest)
	defer waitCancel()
	select {
	case state := <-resultCh:
		return state
	case <-waitCtx.Done():
		placeholder := emptySessionContextState()
		placeholder.Status = "probing"
		return placeholder
	}
}

func (s *Server) contextUsageStateCache() *contextUsageStateCache {
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.contextUsageCache == nil {
		s.contextUsageCache = newContextUsageStateCache(30 * time.Second)
	}
	return s.contextUsageCache
}

func (s *Server) contextUsageProbeFn() contextUsageProbeFunc {
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.contextUsageProbe != nil {
		return s.contextUsageProbe
	}
	return s.probeContextUsageState
}

func (s *Server) refreshContextUsageState(ctx context.Context, sess *session.Session) (sessionContextState, error) {
	probe := s.contextUsageProbeFn()
	return s.contextUsageStateCache().Refresh(ctx, sess, daemonNow(), probe)
}

func (s *Server) probeContextUsageState(ctx context.Context, sess *session.Session) (sessionContextState, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)
	release, err := s.acquireContextRefreshPermit(ctx)
	if err != nil {
		return emptySessionContextState(), err
	}
	defer release()
	prober, ok := contextusage.Get(string(sess.ProviderID()))
	if !ok {
		err := fmt.Errorf("no context-usage prober registered for provider %q", sess.ProviderID())
		s.log.WarnContext(ctx, "daemon.context_usage.probe_unregistered", "component", "daemon", "provider", string(sess.ProviderID()))
		return emptySessionContextState(), err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	usage, err := prober.Probe(probeCtx, sess.Metadata.ProviderSessionID(), contextusage.ProbeOptions{
		RefreshHint: false,
		WorkDir:     sess.Metadata.WorkspaceRoot,
	})
	if err != nil {
		s.log.WarnContext(ctx, "daemon.context_usage.probe_failed", "component", "daemon", "err", err)
		return emptySessionContextState(), fmt.Errorf("probe context usage: %w", err)
	}
	return sessionContextState{
		Usage: contextusage.Usage{
			Snapshot:   usage,
			CapturedAt: daemonNow().UTC(),
			Source:     contextusage.SourceProbe,
		},
		Loaded:     true,
		Status:     "",
		Refreshing: false,
		RetryAfter: time.Time{},
	}, nil
}

func (s *Server) acquireContextRefreshPermit(ctx context.Context) (func(), error) {
	if s.contextRefreshSem == nil {
		return func() {}, nil
	}
	select {
	case s.contextRefreshSem <- contextRefreshPermit{Acquired: true}:
		return func() { <-s.contextRefreshSem }, nil
	case <-ctx.Done():
		s.log.WarnContext(ctx, "daemon.context_usage.refresh_permit_cancelled", "component", "daemon", "err", ctx.Err())
		return nil, fmt.Errorf("acquire context refresh permit: %w", ctx.Err())
	}
}

func (s *Server) renameContextState(oldName, newName string) {
	if oldName == "" || newName == "" || oldName == newName {
		return
	}
	s.contextMu.Lock()
	if state, ok := s.contextStates[oldName]; ok {
		s.contextStates[newName] = state
		delete(s.contextStates, oldName)
	}
	s.contextMu.Unlock()
	if s.contextUsageCache != nil {
		s.contextUsageCache.RenameSession(oldName, newName)
	}
}

func (s *Server) deleteContextState(name string) {
	if name == "" {
		return
	}
	s.contextMu.Lock()
	delete(s.contextStates, name)
	s.contextMu.Unlock()
	if s.contextUsageCache != nil {
		s.contextUsageCache.DeleteSession(name)
	}
}

func emptyInspectStats() inspectStats {
	return inspectStats{
		VisibleMessages:       0,
		VisibleTokensEstimate: 0,
		LastMessageTokens:     0,
		CompactionCount:       0,
		LastPreCompactTokens:  0,
	}
}

func (s *Server) sessionSummary(ctx context.Context, store *session.FileStore, sess *session.Session) *clydev1.SessionSummary {
	caps := sess.SessionProviderCapabilities()
	settings, _ := sessionsettings.Load(store, sess)
	model := "-"
	if caps.TranscriptExport && sess.Metadata.ProviderTranscriptPath() != "" {
		if m := inspectExtractModel(sess.Metadata.ProviderTranscriptPath()); m != "" {
			model = m
		}
	}
	if model == "-" && settings != nil && settings.Model != "" {
		model = adaptercursor.NormalizeSessionSettingsModel(settings.Model)
	}
	stats := emptyInspectStats()
	if caps.TranscriptExport {
		stats = inspectStatsForSession(sess)
	}
	size := int64(0)
	lastActivity := sess.Metadata.LastAccessed
	if p := sess.Metadata.ProviderTranscriptPath(); caps.TranscriptExport && p != "" {
		if info, err := os.Stat(p); err == nil {
			size = info.Size()
			if info.ModTime().After(lastActivity) {
				lastActivity = info.ModTime()
			}
		}
	}
	var bridge *clydev1.Bridge
	if caps.RemoteControl && sess.Metadata.ProviderSessionID() != "" {
		s.bridgeMu.RLock()
		if b := s.bridges[sess.Metadata.ProviderSessionID()]; b != nil {
			if cp, ok := proto.Clone(b).(*clydev1.Bridge); ok {
				bridge = cp
			}
		}
		s.bridgeMu.RUnlock()
	}
	runtime := s.providerRuntimeBoundary(sess, settings, bridge)
	contextState := s.contextStateForSession(ctx, sess)
	lastAutoNameNanos := int64(0)
	if !sess.Metadata.LastAutoNameAt.IsZero() {
		lastAutoNameNanos = sess.Metadata.LastAutoNameAt.UnixNano()
	}
	return &clydev1.SessionSummary{
		Name:                  sess.Name,
		MetadataName:          sess.Metadata.Name,
		ClydeUuid:             sess.ClydeUUID(),
		SessionId:             sess.Metadata.ProviderSessionID(),
		TranscriptPath:        sess.Metadata.ProviderTranscriptPath(),
		WorkDir:               sess.Metadata.WorkDir,
		CreatedNanos:          sess.Metadata.Created.UnixNano(),
		LastAccessedNanos:     sess.Metadata.LastAccessed.UnixNano(),
		ParentSession:         sess.Metadata.ParentSession,
		ParentClydeUuid:       sess.Metadata.ParentClydeUUID,
		IsForkedSession:       sess.Metadata.IsForkedSession,
		IsIncognito:           sess.Metadata.IsIncognito,
		PreviousSessionIds:    append([]string(nil), sess.Metadata.PreviousProviderSessionIDStrings()...),
		Context:               sess.Metadata.Context,
		HasCustomOutputStyle:  sess.Metadata.HasCustomOutputStyle,
		WorkspaceRoot:         sess.Metadata.WorkspaceRoot,
		ContextMessageCount:   compactInt32(sess.Metadata.ContextMessageCount),
		DisplayTitle:          sess.Metadata.Title,
		Model:                 model,
		RemoteControl:         settings != nil && settings.RemoteControl,
		MessageCount:          compactInt32(stats.VisibleMessages),
		TranscriptSizeBytes:   size,
		LastActivityNanos:     lastActivity.UnixNano(),
		Bridge:                bridge,
		ContextTotalTokens:    compactInt32(contextState.Usage.TotalTokens),
		ContextMaxTokens:      compactInt32(contextState.Usage.MaxTokens),
		ContextPercentage:     compactInt32(contextState.Usage.Percentage),
		ContextMessagesTokens: compactInt32(contextState.Usage.CategoryTokens("Messages")),
		ContextUsageLoaded:    contextState.Loaded,
		ContextUsageStatus:    contextState.Status,
		Provider:              string(sess.ProviderID()),
		Runtime:               protoRuntimeBoundary(runtime),
		AutoNameState:         sess.Metadata.AutoNameState.String(),
		AutoNameSource:        sess.Metadata.AutoNameSource.String(),
		LastAutoNameNanos:     lastAutoNameNanos,
		AutoNameSourceHash:    sess.Metadata.AutoNameSourceHash,
	}
}

func (s *Server) sessionDetail(ctx context.Context, store *session.FileStore, sess *session.Session) *clydev1.GetSessionDetailResponse {
	caps := sess.SessionProviderCapabilities()
	settings, _ := sessionsettings.Load(store, sess)
	model := "-"
	if caps.TranscriptExport && sess.Metadata.ProviderTranscriptPath() != "" {
		if m := inspectExtractModel(sess.Metadata.ProviderTranscriptPath()); m != "" {
			model = m
		}
	}
	if model == "-" {
		if settings != nil && settings.Model != "" {
			model = adaptercursor.NormalizeSessionSettingsModel(settings.Model)
		}
	}
	stats := emptyInspectStats()
	if caps.TranscriptExport {
		stats = inspectStatsForSession(sess)
	}
	contextState := s.lazyContextStateForDetail(ctx, sess)
	resp := &clydev1.GetSessionDetailResponse{
		SessionName:           sess.Name,
		Model:                 model,
		TotalMessages:         compactInt32(stats.VisibleMessages),
		VisibleTokensEstimate: compactInt32(stats.VisibleTokensEstimate),
		LastMessageTokens:     compactInt32(stats.LastMessageTokens),
		CompactionCount:       compactInt32(stats.CompactionCount),
		LastPreCompactTokens:  compactInt32(stats.LastPreCompactTokens),
		Provider:              string(sess.ProviderID()),
		Runtime:               protoRuntimeBoundary(s.providerRuntimeBoundary(sess, settings, nil)),
		ContextTotalTokens:    compactInt32(contextState.Usage.TotalTokens),
		ContextMaxTokens:      compactInt32(contextState.Usage.MaxTokens),
		ContextPercentage:     compactInt32(contextState.Usage.Percentage),
		ContextMessagesTokens: compactInt32(contextState.Usage.CategoryTokens("Messages")),
		ContextUsageLoaded:    contextState.Loaded,
		ContextUsageStatus:    contextState.Status,
		ResumeInstructions:    session.ResumeInstructions(sess),
	}
	if p := sess.Metadata.ProviderTranscriptPath(); caps.TranscriptExport && p != "" {
		if info, err := os.Stat(p); err == nil {
			resp.TranscriptSizeBytes = info.Size()
			resp.LastActivityNanos = info.ModTime().UnixNano()
		}
	}
	if caps.TranscriptExport {
		for _, m := range inspectRecentMessages(sess.Metadata.ProviderTranscriptPath(), 5, 150) {
			text := strings.TrimSpace(m.Text)
			if text == "" || strings.HasPrefix(text, "<") || len(text) < 5 {
				continue
			}
			resp.RecentMessages = append(resp.RecentMessages, detailMessageProto(m.Role, text, m.Timestamp))
		}
		for _, m := range inspectAllMessages(sess.Metadata.ProviderTranscriptPath(), 1000) {
			resp.AllMessages = append(resp.AllMessages, detailMessageProto(m.Role, m.Text, m.Timestamp))
		}
		for _, t := range inspectToolUseStats(sess.Metadata.ProviderTranscriptPath(), 8) {
			resp.Tools = append(resp.Tools, &clydev1.ToolUse{Name: t.Name, Count: compactInt32(t.Count)})
		}
	}
	if sess.ProviderID() == session.ProviderCodex {
		s.applyCodexSessionDetail(sess, resp)
	}
	return resp
}

func (s *Server) applyCodexSessionDetail(sess *session.Session, resp *clydev1.GetSessionDetailResponse) {
	path := sess.Metadata.ProviderTranscriptPath()
	if path == "" {
		return
	}
	history, err := itranscript.ReadCodexHistory(path)
	if err != nil {
		s.log.Warn("daemon.codex.detail_read_failed",
			"component", "daemon",
			"session", sess.Name,
			"path", path,
			"err", err,
		)
		return
	}
	if history.ModelProvider != "" && resp.GetModel() == "-" {
		resp.Model = history.ModelProvider
	}
	turns := history.ConversationTurns(itranscript.ShapeOptions{
		ConversationOnly: true,
		IncludeThinking:  false,
		MaxTextRunes:     0,
		ToolOnly:         itranscript.ToolOnlyOmit,
	})
	resp.TotalMessages = compactInt32(len(turns))
	resp.AllMessages = nil
	resp.RecentMessages = nil
	for _, msg := range turns {
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			continue
		}
		resp.AllMessages = append(resp.AllMessages, detailMessageProto(msg.Role, text, msg.Timestamp))
	}
	start := max(len(turns)-5, 0)
	for _, msg := range turns[start:] {
		text := strings.TrimSpace(msg.Text)
		if text == "" {
			continue
		}
		resp.RecentMessages = append(resp.RecentMessages, detailMessageProto(msg.Role, text, msg.Timestamp))
	}
	if info, err := os.Stat(path); err == nil {
		resp.TranscriptSizeBytes = info.Size()
		resp.LastActivityNanos = info.ModTime().UnixNano()
	}
}

func detailMessageProto(role, text string, ts time.Time) *clydev1.DetailMessage {
	out := &clydev1.DetailMessage{Role: role, Text: text}
	if !ts.IsZero() {
		out.TimestampNanos = ts.UnixNano()
	}
	return out
}

func (s *Server) providerRuntimeBoundary(sess *session.Session, settings *session.Settings, bridge *clydev1.Bridge) session.ProviderRuntimeBoundary {
	runtime := sess.ProviderRuntimeBoundary()
	runtime.Live.RemoteControlOn = settings != nil && settings.RemoteControl
	if bridge == nil && runtime.Live.SessionID != "" {
		s.bridgeMu.RLock()
		if b := s.bridges[runtime.Live.SessionID]; b != nil {
			bridge = b
		}
		s.bridgeMu.RUnlock()
	}
	if bridge != nil {
		runtime.Live.BridgeSessionID = bridge.GetBridgeSessionId()
		runtime.Live.BridgeURL = bridge.GetUrl()
	}
	return runtime
}

func protoRuntimeBoundary(runtime session.ProviderRuntimeBoundary) *clydev1.ProviderRuntimeBoundary {
	return &clydev1.ProviderRuntimeBoundary{
		History: protoHistoryBoundary(runtime.History),
		Live:    protoLiveBoundary(runtime.Live),
	}
}

func protoHistoryBoundary(history session.SessionHistoryBoundary) *clydev1.SessionHistoryBoundary {
	previous := make([]*clydev1.ProviderSessionIdentity, 0, len(history.PreviousSessionIDs))
	for _, previousID := range history.PreviousSessionIDs {
		previous = append(previous, &clydev1.ProviderSessionIdentity{
			Provider:  string(history.Provider),
			SessionId: previousID,
		})
	}
	return &clydev1.SessionHistoryBoundary{
		Current: &clydev1.ProviderSessionIdentity{
			Provider:  string(history.Provider),
			SessionId: history.CurrentSessionID,
		},
		Previous:        previous,
		PrimaryArtifact: history.PrimaryArtifact,
		Readable:        history.Readable,
		Exportable:      history.Exportable,
	}
}

func protoLiveBoundary(live session.LiveSessionBoundary) *clydev1.LiveSessionBoundary {
	return &clydev1.LiveSessionBoundary{
		Current: &clydev1.ProviderSessionIdentity{
			Provider:  string(live.Provider),
			SessionId: live.SessionID,
		},
		TailReadable:    live.TailReadable,
		InputWritable:   live.InputWritable,
		RemoteControl:   live.RemoteControlOn,
		BridgeSessionId: live.BridgeSessionID,
		BridgeUrl:       live.BridgeURL,
	}
}

// settingsLockFor returns the per-session mutex used to serialise
// settings.json writes. The lock is created lazily so cold sessions
// do not occupy memory.
func (s *Server) settingsLockFor(name string) *sync.Mutex {
	s.settingsLocksMu.Lock()
	defer s.settingsLocksMu.Unlock()
	if m, ok := s.settingsLocks[name]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.settingsLocks[name] = m
	return m
}

// UpdateSessionSettings is the daemon side write path for per session
// settings.json. Mutations from the TUI, CLI, and any other client
// go through here so writes serialise per session and broadcast to
// every subscriber on completion.
func (s *Server) UpdateSessionSettings(ctx context.Context, req *clydev1.UpdateSessionSettingsRequest) (*clydev1.UpdateSessionSettingsResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Get(req.GetName())
	if err != nil || sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetName())
	}
	caps := sess.SessionProviderCapabilities()
	if !caps.PerSessionSettings {
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support per-session settings", sess.ProviderID())
	}
	if req.GetSettings() != nil && maskApplies(req.GetUpdateMask(), "remote_control") && !caps.RemoteControl {
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support remote control", sess.ProviderID())
	}
	lock := s.settingsLockFor(req.GetName())
	lock.Lock()
	defer lock.Unlock()

	current, _ := sessionsettings.Load(store, sess)
	if current == nil {
		current = defaultSessionSettings(false)
	}
	applyMask := func(field string) bool {
		if len(req.GetUpdateMask()) == 0 {
			return true
		}
		return slices.Contains(req.GetUpdateMask(), field)
	}
	settings := req.GetSettings()
	if settings != nil && applyMask("model") {
		current.Model = adaptercursor.NormalizeSessionSettingsModel(settings.GetModel())
	}
	if settings != nil && applyMask("effort_level") {
		current.EffortLevel = settings.GetEffortLevel()
	}
	if settings != nil && applyMask("output_style") {
		current.OutputStyle = settings.GetOutputStyle()
	}
	if settings != nil && applyMask("remote_control") {
		current.RemoteControl = settings.GetRemoteControl()
	}
	if settings != nil && applyMask("context_window") {
		current.ContextWindow = settings.GetContextWindow()
	}
	if err := sessionsettings.Save(store, sess, current); err != nil {
		return nil, status.Errorf(codes.Internal, "save settings: %v", err)
	}
	s.publishSessionSummaryEvent(ctx, clydev1.SubscribeRegistryResponse_KIND_SESSION_UPDATED, store, sess, "")
	s.log.LogAttrs(ctx, slog.LevelInfo, "session settings updated via RPC",
		slog.String("session", req.GetName()),
		slog.Bool("remote_control", current.RemoteControl),
		slog.String("model", current.Model),
		slog.String("cursor_normalized_model", adaptercursor.NormalizeModelAlias(current.Model)),
		slog.String("effort", current.EffortLevel),
		slog.String("context_window", current.ContextWindow),
	)
	return &clydev1.UpdateSessionSettingsResponse{}, nil
}

func maskApplies(mask []string, field string) bool {
	return len(mask) == 0 || slices.Contains(mask, field)
}

// UpdateGlobalSettings mutates the clyde global config defaults
// from any client. Currently only the remote_control default is
// exposed because that is the field this work needs. The handler
// rewrites the global config TOML through internal/config so callers
// do not need filesystem access.
func (s *Server) UpdateGlobalSettings(ctx context.Context, req *clydev1.UpdateGlobalSettingsRequest) (*clydev1.UpdateGlobalSettingsResponse, error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load global: %v", err)
	}
	applyMask := func(field string) bool {
		if len(req.GetUpdateMask()) == 0 {
			return true
		}
		return slices.Contains(req.GetUpdateMask(), field)
	}
	if req.GetDefaults() != nil && applyMask("remote_control") {
		cfg.Defaults.RemoteControl = req.GetDefaults().GetRemoteControl()
	}
	if err := config.SaveGlobal(cfg); err != nil {
		return nil, status.Errorf(codes.Internal, "save global: %v", err)
	}
	s.publishGlobalSettingsEvent(cfg.Defaults.RemoteControl)
	s.log.LogAttrs(ctx, slog.LevelInfo, "global settings updated via RPC",
		slog.Bool("remote_control", cfg.Defaults.RemoteControl),
	)
	return &clydev1.UpdateGlobalSettingsResponse{}, nil
}

// ListConfigControls returns daemon-editable configuration controls.
func (s *Server) ListConfigControls(ctx context.Context, _ *clydev1.ListConfigControlsRequest) (*clydev1.ListConfigControlsResponse, error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load global: %v", err)
	}
	descs := config.ListControlDescriptors(cfg)
	out := make([]*clydev1.ConfigControl, 0, len(descs))
	for _, desc := range descs {
		out = append(out, protoConfigControl(desc))
	}
	s.log.LogAttrs(ctx, slog.LevelDebug, "daemon.config_controls.list",
		slog.String("component", "daemon"),
		slog.Int("count", len(out)),
	)
	return &clydev1.ListConfigControlsResponse{Controls: out}, nil
}

// UpdateConfigControl mutates one daemon-editable configuration control.
func (s *Server) UpdateConfigControl(ctx context.Context, req *clydev1.UpdateConfigControlRequest) (*clydev1.UpdateConfigControlResponse, error) {
	key := strings.TrimSpace(req.GetKey())
	if key == "" {
		return nil, status.Error(codes.InvalidArgument, "key is required")
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load global: %v", err)
	}
	if err := config.UpdateControlValue(cfg, key, req.GetValue()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "update control: %v", err)
	}
	if err := config.SaveGlobal(cfg); err != nil {
		return nil, status.Errorf(codes.Internal, "save global: %v", err)
	}
	if key == "defaults.remote_control" {
		s.publishGlobalSettingsEvent(cfg.Defaults.RemoteControl)
	}
	var updated *clydev1.ConfigControl
	for _, desc := range config.ListControlDescriptors(cfg) {
		if desc.Key == key {
			updated = protoConfigControl(desc)
			break
		}
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "daemon.config_controls.updated",
		slog.String("component", "daemon"),
		slog.String("key", key),
		slog.String("value", strings.TrimSpace(req.GetValue())),
	)
	return &clydev1.UpdateConfigControlResponse{Control: updated}, nil
}

func protoConfigControl(desc config.ControlDescriptor) *clydev1.ConfigControl {
	options := make([]*clydev1.ConfigControlOption, 0, len(desc.Options))
	for _, opt := range desc.Options {
		options = append(options, &clydev1.ConfigControlOption{
			Value:       opt.Value,
			Label:       opt.Label,
			Description: opt.Description,
		})
	}
	return &clydev1.ConfigControl{
		Key:          desc.Key,
		Section:      desc.Section,
		Label:        desc.Label,
		Description:  desc.Description,
		Type:         protoConfigControlType(desc.Type),
		Value:        desc.Value,
		DefaultValue: desc.DefaultValue,
		Options:      options,
		Sensitive:    desc.Sensitive,
		ReadOnly:     desc.ReadOnly,
	}
}

func protoConfigControlType(kind config.ControlType) clydev1.ConfigControlType {
	switch kind {
	case config.ControlTypeBool:
		return clydev1.ConfigControlType_CONFIG_CONTROL_TYPE_BOOL
	case config.ControlTypeEnum:
		return clydev1.ConfigControlType_CONFIG_CONTROL_TYPE_ENUM
	case config.ControlTypeString:
		return clydev1.ConfigControlType_CONFIG_CONTROL_TYPE_STRING
	case config.ControlTypePath:
		return clydev1.ConfigControlType_CONFIG_CONTROL_TYPE_PATH
	default:
		return clydev1.ConfigControlType_CONFIG_CONTROL_TYPE_UNSPECIFIED
	}
}

// createSessionRecord mints a provider session id (when the provider
// pre-assigns one via the session minter registry) and persists the canonical
// session row. It does not launch anything or write launch-specific settings.
// CreateSession and StartRemoteSession both use it so record creation has a
// single daemon-owned path. The id-minting rule stays in the provider package;
// this function only dispatches by provider id.
func createSessionRecord(ctx context.Context, log *slog.Logger, store *session.FileStore, provider session.ProviderID, name, basedir string, incognito bool) (*session.Session, error) {
	provider = session.NormalizeProviderID(provider)
	sessionID, _, err := session.MintSessionID(provider)
	if err != nil {
		log.ErrorContext(ctx, "daemon.create_session.mint_failed",
			"component", "daemon",
			"subcomponent", "sessions",
			"provider", string(provider),
			"err", err,
		)
		return nil, fmt.Errorf("mint session id: %w", err)
	}
	sess := session.NewSession(name, sessionID)
	sess.Metadata.Provider = provider
	sess.Metadata.NormalizeProviderState()
	sess.Metadata.WorkDir = basedir
	sess.Metadata.WorkspaceRoot = basedir
	sess.Metadata.IsIncognito = incognito
	if err := store.Create(sess); err != nil {
		log.ErrorContext(ctx, "daemon.create_session.store_failed",
			"component", "daemon",
			"subcomponent", "sessions",
			"session", name,
			"err", err,
		)
		return nil, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// CreateSession is the daemon-owned record-creation entry point. It mints a
// provider session id, persists the row, and returns it without launching
// anything. The CLI calls this so the daemon is the sole writer of session
// records, then launches the tool locally with the returned id.
func (s *Server) CreateSession(ctx context.Context, req *clydev1.CreateSessionRequest) (*clydev1.CreateSessionResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	basedir := strings.TrimSpace(req.GetBasedir())
	if basedir == "" {
		if basedir, err = os.Getwd(); err != nil {
			return nil, status.Errorf(codes.Internal, "resolve working directory: %v", err)
		}
	}
	if info, statErr := os.Stat(basedir); statErr != nil || !info.IsDir() {
		return nil, status.Errorf(codes.InvalidArgument, "basedir %q is not a directory", basedir)
	}
	provider := session.ProviderClaude
	if raw := strings.TrimSpace(req.GetProvider()); raw != "" {
		provider = session.NormalizeProviderID(session.ProviderID(raw))
	}
	name := strings.TrimSpace(req.GetSessionName())
	if name == "" {
		name, err = nextRemoteSessionName(store)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "allocate session name: %v", err)
		}
	} else if validateErr := session.ValidateDisplayName(name); validateErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid session name %q", name)
	}
	if store.Exists(name) {
		return nil, status.Errorf(codes.AlreadyExists, "session %q already exists", name)
	}
	sess, err := createSessionRecord(ctx, s.log, store, provider, name, basedir, req.GetIncognito())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create session: %v", err)
	}
	s.publishSessionSummaryEvent(ctx, clydev1.SubscribeRegistryResponse_KIND_SESSION_ADOPTED, store, sess, "")
	s.log.InfoContext(ctx, "daemon.create_session.created",
		"component", "daemon",
		"subcomponent", "sessions",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"provider", string(provider),
		"peer_addr", daemonPeerAddr(peerInfo),
	)
	return &clydev1.CreateSessionResponse{
		SessionName: sess.Name,
		SessionId:   sess.Metadata.ProviderSessionID(),
	}, nil
}

// StartRemoteSession creates a canonical clyde session row, persists remote
// control settings, then launches a daemon-owned headless worker that runs
// Claude with --remote-control against the pre-assigned session UUID.
func (s *Server) StartRemoteSession(ctx context.Context, req *clydev1.StartRemoteSessionRequest) (*clydev1.StartRemoteSessionResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	basedir := strings.TrimSpace(req.GetBasedir())
	if basedir == "" {
		if basedir, err = os.Getwd(); err != nil {
			return nil, status.Errorf(codes.Internal, "resolve working directory: %v", err)
		}
	}
	if info, err := os.Stat(basedir); err != nil || !info.IsDir() {
		return nil, status.Errorf(codes.InvalidArgument, "basedir %q is not a directory", basedir)
	}

	requestedName := req.GetSessionName()
	name := strings.TrimSpace(requestedName)
	if name == "" {
		name, err = nextRemoteSessionName(store)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "allocate session name: %v", err)
		}
	} else {
		name = requestedName
		if err := session.ValidateDisplayName(name); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid session name %q", name)
		}
	}
	if store.Exists(name) {
		return nil, status.Errorf(codes.AlreadyExists, "session %q already exists", name)
	}

	sess, err := createSessionRecord(ctx, s.log, store, session.ProviderClaude, name, basedir, req.GetIncognito())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create session: %v", err)
	}
	sessionID := sess.Metadata.ProviderSessionID()
	if err := sessionsettings.Save(store, sess, defaultSessionSettings(true)); err != nil {
		_ = store.Delete(name)
		return nil, status.Errorf(codes.Internal, "save session settings: %v", err)
	}
	s.publishSessionSummaryEvent(ctx, clydev1.SubscribeRegistryResponse_KIND_SESSION_ADOPTED, store, sess, "")

	cmd, err := s.startRemoteWorkerProcess(ctx, name, sessionID, basedir, req.GetIncognito())
	if err != nil {
		_ = store.Delete(name)
		return nil, status.Errorf(codes.Internal, "launch remote session: %v", err)
	}
	worker := &remoteWorker{
		sessionName:      name,
		sessionID:        sessionID,
		basedir:          basedir,
		incognito:        req.GetIncognito(),
		cmd:              cmd,
		done:             make(chan struct{}),
		skipCleanup:      atomic.Bool{},
		livetrackSession: nil,
	}
	s.remoteMu.Lock()
	s.remoteWorkers[remoteWorkerKey(name, sessionID)] = worker
	s.remoteMu.Unlock()
	s.registerClaudeRemoteWorker(ctx, worker)
	workerCtx := daemonDetachedCorrelationContext(ctx, s.log)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WarnContext(workerCtx, "daemon.remote_session.wait_panicked",
					"component", "daemon",
					"session", worker.sessionName,
					"session_id", worker.sessionID,
					"panic", r,
				)
			}
		}()
		s.waitRemoteWorker(workerCtx, worker)
	}()
	s.log.InfoContext(ctx, "daemon.remote_session.started",
		"component", "daemon",
		"session", name,
		"session_id", sessionID,
		"basedir", basedir,
		"incognito", req.GetIncognito(),
		"pid", cmd.Process.Pid,
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD),
	)
	return &clydev1.StartRemoteSessionResponse{
		SessionName: name,
		SessionId:   sessionID,
		LaunchState: clydev1.StartRemoteSessionResponse_LAUNCH_STATE_LAUNCHING,
	}, nil
}

// claudeWorkerPIDs returns the PIDs of every live Claude worker the daemon
// currently tracks through livetrack. The in-use detector calls this to
// exclude daemon-spawned `claude` processes from the external-session count,
// which is how the OAuth rotator decides whether the keychain credential is
// held by an external Claude Code session. A nil receiver or a server with no
// liveWorkers registry yields an empty slice so the detector treats every
// running `claude` process as external, the conservative default.
func (s *Server) claudeWorkerPIDs() []int {
	if s == nil || s.liveWorkers == nil {
		return nil
	}
	snapshot := s.liveWorkers.Snapshot()
	out := make([]int, 0, len(snapshot))
	for _, sess := range snapshot {
		if sess.Meta.Provider != "claude" || sess.Meta.WorkerPID == 0 {
			continue
		}
		out = append(out, sess.Meta.WorkerPID)
	}
	return out
}

// registerClaudeRemoteWorker registers a newly-launched Claude remote worker
// with the livetrack registry.
func (s *Server) registerClaudeRemoteWorker(ctx context.Context, worker *remoteWorker) {
	if worker == nil || worker.cmd == nil || worker.cmd.Process == nil || s.liveWorkers == nil {
		return
	}
	lsess, err := s.liveWorkers.Register(ctx, "claude.remote", LiveMeta{
		Provider:      "claude",
		LiveSessionID: worker.sessionID,
		WorkerPID:     worker.cmd.Process.Pid,
		Lease:         "background",
	}, &claudeWorkerCloser{
		proc:           worker.cmd.Process,
		done:           worker.done,
		interruptGrace: claudeRemoteSuspendTimeout,
	})
	if err == nil {
		worker.livetrackSession = lsess
		return
	}
	s.log.WarnContext(ctx, "daemon.remote_session.livetrack_register_failed",
		"component", "daemon",
		"provider", session.ProviderClaude,
		"session", worker.sessionName,
		"session_id", worker.sessionID,
		"err", err,
	)
}

func defaultSessionSettings(remoteControl bool) *session.Settings {
	return &session.Settings{
		Model:         "",
		EffortLevel:   "",
		OutputStyle:   "",
		RemoteControl: remoteControl,
		ContextWindow: "",
		Permissions: session.Permissions{
			Allow:                        nil,
			Ask:                          nil,
			Deny:                         nil,
			AdditionalDirectories:        nil,
			DefaultMode:                  "",
			DisableBypassPermissionsMode: "",
		},
	}
}

// ListBridges returns the current set of active claude --remote-control
// bridges as discovered by the bridge watcher.
func (s *Server) ListBridges(_ context.Context, _ *clydev1.ListBridgesRequest) (*clydev1.ListBridgesResponse, error) {
	return &clydev1.ListBridgesResponse{Bridges: s.snapshotBridges()}, nil
}

// snapshotBridges returns a copy of the daemon's current bridge map.
// The webapp uses it to render the active bridge list without going
// through the gRPC surface.
func (s *Server) snapshotBridges() []*clydev1.Bridge {
	s.bridgeMu.RLock()
	defer s.bridgeMu.RUnlock()
	out := make([]*clydev1.Bridge, 0, len(s.bridges))
	for _, b := range s.bridges {
		cloned, ok := proto.Clone(b).(*clydev1.Bridge)
		if !ok {
			continue
		}
		out = append(out, cloned)
	}
	return out
}

// setBridge records a bridge entry and broadcasts BRIDGE_OPENED.
func (s *Server) setBridge(b *clydev1.Bridge) {
	if b == nil || b.GetSessionId() == "" {
		return
	}
	s.bridgeMu.Lock()
	prev, exists := s.bridges[b.GetSessionId()]
	s.bridges[b.GetSessionId()] = b
	s.bridgeMu.Unlock()
	if exists && prev != nil && prev.GetBridgeSessionId() == b.GetBridgeSessionId() {
		return
	}
	s.publishEvent(&clydev1.SubscribeRegistryResponse{
		Kind:            clydev1.SubscribeRegistryResponse_KIND_BRIDGE_OPENED,
		SessionId:       b.GetSessionId(),
		BridgeSessionId: b.GetBridgeSessionId(),
		BridgeUrl:       b.GetUrl(),
	})
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "bridge opened",
		slog.String("session_id", b.GetSessionId()),
		slog.String("bridge", b.GetBridgeSessionId()),
	)
}

// removeBridge clears a bridge entry and broadcasts BRIDGE_CLOSED.
func (s *Server) removeBridge(sessionID string) {
	if sessionID == "" {
		return
	}
	s.bridgeMu.Lock()
	prev, ok := s.bridges[sessionID]
	delete(s.bridges, sessionID)
	s.bridgeMu.Unlock()
	if !ok || prev == nil {
		return
	}
	s.publishEvent(&clydev1.SubscribeRegistryResponse{
		Kind:            clydev1.SubscribeRegistryResponse_KIND_BRIDGE_CLOSED,
		SessionId:       sessionID,
		BridgeSessionId: prev.GetBridgeSessionId(),
		BridgeUrl:       prev.GetUrl(),
	})
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "bridge closed",
		slog.String("session_id", sessionID),
		slog.String("bridge", prev.GetBridgeSessionId()),
	)
}

type resolvedSessionRuntime struct {
	Session         *session.Session
	Provider        session.ProviderID
	SessionID       string
	HistoryArtifact string
}

func emptyResolvedSessionRuntime() resolvedSessionRuntime {
	return resolvedSessionRuntime{
		Session:         nil,
		Provider:        session.ProviderUnknown,
		SessionID:       "",
		HistoryArtifact: "",
	}
}

func resolveSessionRuntime(sessionName, provider, sessionID string) (resolvedSessionRuntime, error) {
	store, err := session.NewGlobalFileStore()
	if err != nil {
		slog.Warn("daemon.resolve_session_runtime.store_failed", "err", err)
		return emptyResolvedSessionRuntime(), fmt.Errorf("new global session store: %w", err)
	}
	providerID := session.ProviderID(strings.TrimSpace(provider))
	sess, err := resolveStoredSession(store, sessionName, sessionID)
	if err != nil {
		slog.Warn("daemon.resolve_session_runtime.resolve_failed", "session", sessionName, "session_id", sessionID, "err", err)
		return emptyResolvedSessionRuntime(), fmt.Errorf("resolve stored session: %w", err)
	}
	if sess == nil {
		return emptyResolvedSessionRuntime(), nil
	}
	runtime := sess.ProviderRuntimeBoundary()
	if providerID != session.ProviderUnknown && session.NormalizeProviderID(providerID) != runtime.Live.Provider {
		return emptyResolvedSessionRuntime(), nil
	}
	return resolvedSessionRuntime{
		Session:         sess,
		Provider:        runtime.Live.Provider,
		SessionID:       runtime.Live.SessionID,
		HistoryArtifact: runtime.History.PrimaryArtifact,
	}, nil
}

// TailTranscript streams provider history lines for a session via the hub.
// Reference counted so multiple subscribers share one underlying
// fsnotify watcher per provider history artifact. Legacy callers may still
// pass session_id only; generic callers can pass session_name and provider.
func (s *Server) TailTranscript(req *clydev1.TailTranscriptRequest, stream clydev1.ClydeService_TailTranscriptServer) error {
	if req.GetSessionId() == "" && req.GetSessionName() == "" {
		return status.Error(codes.InvalidArgument, "session_id or session_name is required")
	}
	target, err := resolveSessionRuntime(req.GetSessionName(), req.GetProvider(), req.GetSessionId())
	if err != nil {
		return status.Errorf(codes.Internal, "resolve session runtime: %v", err)
	}
	if target.Session != nil && !target.Session.ProviderRuntimeBoundary().Live.TailReadable {
		return status.Errorf(codes.FailedPrecondition, "session provider %q does not support live tailing", target.Session.ProviderID())
	}
	if target.Session == nil {
		return status.Errorf(codes.NotFound, "no session runtime for %q", req.GetSessionId())
	}
	if !target.Session.SessionProviderCapabilities().TranscriptTail {
		return status.Errorf(codes.FailedPrecondition, "session provider %q does not support transcript tailing", target.Session.ProviderID())
	}
	path := target.HistoryArtifact
	if path == "" {
		return status.Errorf(codes.NotFound, "no history artifact for session %q", target.SessionID)
	}
	startOffset := req.GetStartAtOffset()
	if startOffset == 0 {
		// Default to streaming future lines only. Callers that want
		// the full file pass start_at_offset = 1 (effectively any
		// nonzero positive value before the file size).
		startOffset = -1
	}
	ch, cleanup, err := s.transcripts.Subscribe(target.SessionID, path, startOffset)
	if err != nil {
		return status.Errorf(codes.Internal, "open tailer: %v", err)
	}
	defer cleanup()

	ctx := stream.Context()
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return nil
			}
			line.Provider = string(target.Provider)
			line.SessionId = target.SessionID
			if target.Session != nil {
				line.SessionName = target.Session.Name
			}
			if err := stream.Send(line); err != nil {
				s.log.WarnContext(ctx, "daemon.tail_transcript.send_failed", "err", err)
				return fmt.Errorf("send transcript line: %w", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// SendToSession delivers text into a running provider session via the
// provider runtime input channel. Claude compatibility uses the per-session
// inject socket the wrapper opened on launch. Returns
// delivered=false when no socket exists, so callers can fall back to
// telling the user to use the local terminal directly.
func (s *Server) SendToSession(ctx context.Context, req *clydev1.SendToSessionRequest) (*clydev1.SendToSessionResponse, error) {
	if req.GetSessionId() == "" && req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id or session_name is required")
	}
	target, err := resolveSessionRuntime(req.GetSessionName(), req.GetProvider(), req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session runtime: %v", err)
	}
	if target.Session != nil && !target.Session.ProviderRuntimeBoundary().Live.InputWritable {
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support live input", target.Session.ProviderID())
	}
	if target.Session == nil {
		return nil, status.Errorf(codes.NotFound, "no session runtime for %q", req.GetSessionId())
	}
	if !target.Session.SessionProviderCapabilities().RemoteControl {
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support remote control", target.Session.ProviderID())
	}
	socketPath := injectSocketPath(target.SessionID)
	if _, err := os.Stat(socketPath); err != nil {
		return &clydev1.SendToSessionResponse{Delivered: false}, nil
	}
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return &clydev1.SendToSessionResponse{Delivered: false}, nil
	}
	defer func() { _ = conn.Close() }()
	payload := req.GetText()
	if !strings.HasSuffix(payload, "\n") {
		payload += "\n"
	}
	n, werr := conn.Write([]byte(payload))
	if werr != nil {
		return &clydev1.SendToSessionResponse{Delivered: false, BytesWritten: compactInt32(n)}, nil
	}
	return &clydev1.SendToSessionResponse{Delivered: true, BytesWritten: compactInt32(n)}, nil
}

func nextRemoteSessionName(store *session.FileStore) (string, error) {
	list, err := store.List()
	if err != nil {
		slog.Warn("daemon.next_remote_session_name.list_failed", "err", err)
		return "", fmt.Errorf("list sessions: %w", err)
	}
	taken := make(map[string]bool, len(list))
	for _, sess := range list {
		if sess == nil {
			continue
		}
		taken[sess.Name] = true
	}
	base := "chat-" + daemonNow().UTC().Format("20060102-150405")
	name := session.UniqueDisplayName(base, taken)
	if name == "" || taken[name] {
		return "", fmt.Errorf("could not allocate a unique session name")
	}
	return name, nil
}

func (s *Server) startRemoteWorkerProcess(ctx context.Context, sessionName, sessionID, basedir string, incognito bool) (*exec.Cmd, error) {
	_, _ = peer.FromContext(ctx)
	_, _ = metadata.FromIncomingContext(ctx)

	self, err := remoteWorkerExecutable()
	if err != nil {
		return nil, fmt.Errorf("resolve remote worker executable: %w", err)
	}
	tmux := detectRemoteWorkerTmuxState()
	if tmux.Detected {
		s.log.WarnContext(ctx, "daemon.remote_session.tmux_unsupported",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"session", sessionName,
			"session_id", sessionID,
			"basedir", basedir,
			"tmux_status", tmux.Status,
			"reason", tmux.Reason,
		)
	}
	args := []string{
		"daemon",
		"launch-remote-worker",
		"--session-name", sessionName,
		"--session-id", sessionID,
		"--basedir", basedir,
	}
	if incognito {
		args = append(args, "--incognito")
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0o666)
	if err != nil {
		return nil, fmt.Errorf("open dev null: %w", err)
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), self, args...)
	cmd.Dir = basedir
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		_ = devNull.Close()
		return nil, fmt.Errorf("start remote worker: %w", err)
	}
	_ = devNull.Close()
	s.log.InfoContext(ctx, "daemon.remote_session.worker_launched",
		"component", "daemon",
		"provider", session.ProviderClaude,
		"session", sessionName,
		"session_id", sessionID,
		"basedir", basedir,
		"incognito", incognito,
		"pid", cmd.Process.Pid,
		"tmux_status", tmux.Status,
	)
	return cmd, nil
}

func detectRemoteWorkerTmuxState() remoteWorkerTmuxState {
	if strings.TrimSpace(os.Getenv("TMUX")) == "" {
		return remoteWorkerTmuxState{Detected: false, Status: "not_detected", Reason: ""}
	}
	return remoteWorkerTmuxState{
		Detected: true,
		Status:   "unsupported",
		Reason:   "daemon-owned Claude live sessions run headless and are restored through foreground leases, not tmux attachment",
	}
}

func (s *Server) waitRemoteWorker(ctx context.Context, worker *remoteWorker) {
	if worker == nil || worker.cmd == nil {
		return
	}
	err := worker.cmd.Wait()
	if worker.done != nil {
		close(worker.done)
	}
	s.remoteMu.Lock()
	removeRemoteWorkerLocked(s.remoteWorkers, worker)
	s.remoteMu.Unlock()
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
	}
	s.log.LogAttrs(ctx, level, "daemon.remote_session.exited",
		slog.String("component", "daemon"),
		slog.String("session", worker.sessionName),
		slog.String("session_id", worker.sessionID),
		slog.Bool("incognito", worker.incognito),
		slog.Any("err", err),
	)
	if !worker.incognito || worker.skipCleanup.Load() {
		return
	}
	store, storeErr := session.NewGlobalFileStore()
	deleteName := worker.sessionName
	if storeErr == nil {
		deleteName = firstNonEmpty(resolvedSessionName(store, worker.sessionName, worker.sessionID), deleteName)
	}
	if _, delErr := s.DeleteSession(ctx, &clydev1.DeleteSessionRequest{Name: deleteName}); delErr != nil {
		s.log.WarnContext(ctx, "daemon.remote_session.incognito_cleanup_failed",
			"component", "daemon",
			"session", deleteName,
			"session_id", worker.sessionID,
			"err", delErr,
		)
	}
}

// ProbeContextUsage refreshes and returns live context usage for one session.
func (s *Server) ProbeContextUsage(ctx context.Context, req *clydev1.ProbeContextUsageRequest) (*clydev1.ProbeContextUsageResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name is required")
	}
	started := daemonNow()
	s.log.InfoContext(ctx, "daemon.context_usage.probe.started",
		"component", "daemon",
		"subcomponent", "context_usage",
		"session", req.GetSessionName(),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD))
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	state, err := s.refreshContextUsageState(ctx, sess)
	if err != nil {
		s.log.WarnContext(ctx, "daemon.context_usage.probe.failed",
			"component", "daemon",
			"subcomponent", "context_usage",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			"duration_ms", time.Since(started).Milliseconds(),
			"err", err)
		return nil, status.Errorf(codes.Internal, "probe context usage: %v", err)
	}
	usage := state.Usage
	resp := &clydev1.ProbeContextUsageResponse{
		SessionName: sess.Name,
		SessionId:   sess.Metadata.ProviderSessionID(),
		Model:       usage.Model,
		TotalTokens: compactInt32(usage.TotalTokens),
		MaxTokens:   compactInt32(usage.MaxTokens),
		Percentage:  compactInt32(usage.Percentage),
		Categories:  make([]*clydev1.ContextUsageCategory, 0, len(usage.Categories)),
	}
	for _, cat := range usage.Categories {
		resp.Categories = append(resp.Categories, &clydev1.ContextUsageCategory{
			Name:       cat.Name,
			Tokens:     compactInt32(cat.Tokens),
			IsDeferred: cat.IsDeferred,
		})
	}
	s.log.InfoContext(ctx, "daemon.context_usage.probe.completed",
		"component", "daemon",
		"subcomponent", "context_usage",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"duration_ms", time.Since(started).Milliseconds(),
		"context_total", usage.TotalTokens,
		"context_limit", usage.MaxTokens,
		"percentage", usage.Percentage)
	return resp, nil
}

// CalibrateSession probes the session's live context-usage view through
// the registered provider Prober, derives static_overhead from the
// resulting Snapshot, persists the calibration via
// compactengine.SaveCalibration, and returns the stored record.
func (s *Server) CalibrateSession(ctx context.Context, req *clydev1.CalibrateSessionRequest) (*clydev1.CalibrateSessionResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name is required")
	}
	started := daemonNow()
	s.log.InfoContext(ctx, "daemon.calibrate.started",
		"component", "daemon",
		"subcomponent", "calibrate",
		"session", req.GetSessionName(),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD))
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	prober, ok := contextusage.Get(string(sess.ProviderID()))
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "no context-usage prober registered for provider %q", sess.ProviderID())
	}
	snapshot, err := prober.Probe(ctx, sess.Metadata.ProviderSessionID(), contextusage.ProbeOptions{
		RefreshHint: false,
		WorkDir:     sess.Metadata.WorkspaceRoot,
	})
	if err != nil {
		s.log.WarnContext(ctx, "daemon.calibrate.probe_failed",
			"component", "daemon",
			"subcomponent", "calibrate",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			"duration_ms", time.Since(started).Milliseconds(),
			"err", err)
		return nil, status.Errorf(codes.Internal, "probe context usage: %v", err)
	}
	overhead := snapshot.StaticOverhead()
	if overhead <= 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "derived static_overhead was zero (total=%d); refusing to save", snapshot.TotalTokens)
	}
	cal := compactengine.Calibration{
		StaticOverhead: overhead,
		CapturedAt:     daemonNow().UTC(),
		Model:          snapshot.Model,
	}
	if err := compactengine.SaveCalibration(sess.Metadata.ProviderSessionID(), cal); err != nil {
		return nil, status.Errorf(codes.Internal, "save calibration: %v", err)
	}
	s.log.InfoContext(ctx, "daemon.calibrate.completed",
		"component", "daemon",
		"subcomponent", "calibrate",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"static_overhead", overhead,
		"model", cal.Model,
		"duration_ms", time.Since(started).Milliseconds())
	return &clydev1.CalibrateSessionResponse{
		SessionName:     sess.Name,
		SessionId:       sess.Metadata.ProviderSessionID(),
		Calibration:     calibrationRecordToProto(cal),
		TotalTokens:     compactInt32(snapshot.TotalTokens),
		CategoriesCount: compactInt32(len(snapshot.Categories)),
	}, nil
}

// SetCalibration persists an operator-supplied static_overhead value
// for the session via compactengine.SaveCalibration and returns the
// stored record. It does not probe the live session.
func (s *Server) SetCalibration(ctx context.Context, req *clydev1.SetCalibrationRequest) (*clydev1.SetCalibrationResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name is required")
	}
	if req.GetStaticOverhead() < 0 {
		return nil, status.Error(codes.InvalidArgument, "static_overhead must be >= 0")
	}
	started := daemonNow()
	s.log.InfoContext(ctx, "daemon.set_calibration.started",
		"component", "daemon",
		"subcomponent", "calibrate",
		"session", req.GetSessionName(),
		"static_overhead", req.GetStaticOverhead(),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD))
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	cal := compactengine.Calibration{
		StaticOverhead: int(req.GetStaticOverhead()),
		CapturedAt:     daemonNow().UTC(),
		Model:          "",
	}
	if err := compactengine.SaveCalibration(sess.Metadata.ProviderSessionID(), cal); err != nil {
		return nil, status.Errorf(codes.Internal, "save calibration: %v", err)
	}
	s.log.InfoContext(ctx, "daemon.set_calibration.completed",
		"component", "daemon",
		"subcomponent", "calibrate",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"static_overhead", cal.StaticOverhead,
		"duration_ms", time.Since(started).Milliseconds())
	return &clydev1.SetCalibrationResponse{
		SessionName: sess.Name,
		SessionId:   sess.Metadata.ProviderSessionID(),
		Calibration: calibrationRecordToProto(cal),
	}, nil
}

// calibrationRecordToProto converts a compact.Calibration to its
// wire envelope shape. Centralizing the conversion keeps the daemon
// handlers and any future caller in lock step with the proto fields.
func calibrationRecordToProto(cal compactengine.Calibration) *clydev1.CalibrationRecord {
	return &clydev1.CalibrationRecord{
		StaticOverhead: int64(cal.StaticOverhead),
		CapturedAt:     timestamppb.New(cal.CapturedAt),
		Model:          cal.Model,
	}
}

// CompactPreview streams compact preview planning events for one session.
func (s *Server) CompactPreview(
	req *clydev1.CompactRunRequest,
	stream clydev1.ClydeService_CompactPreviewServer,
) error {
	ctx := stream.Context()
	s.log.InfoContext(ctx, "daemon.compact.preview.started",
		"component", "daemon",
		"subcomponent", "compact",
		"session", req.GetSessionName(),
		"target", req.GetTargetTokens(),
	)
	return s.runCompact(ctx, req, stream, compactengine.RuntimeModePreview)
}

// CompactApply streams compact planning and apply events for one session.
func (s *Server) CompactApply(
	req *clydev1.CompactRunRequest,
	stream clydev1.ClydeService_CompactApplyServer,
) error {
	ctx := stream.Context()
	s.log.InfoContext(ctx, "daemon.compact.apply.started",
		"component", "daemon",
		"subcomponent", "compact",
		"session", req.GetSessionName(),
		"target", req.GetTargetTokens(),
	)
	return s.runCompact(ctx, req, stream, compactengine.RuntimeModeApply)
}

func (s *Server) runCompact(
	ctx context.Context,
	req *clydev1.CompactRunRequest,
	stream compactEventStream,
	mode compactengine.RuntimeMode,
) error {
	s.log.DebugContext(ctx, "daemon.compact.run.begin",
		"component", "daemon",
		"subcomponent", "compact",
		"session", req.GetSessionName(),
		"target", req.GetTargetTokens(),
		"mode", mode,
	)
	run, err := s.prepareCompactRun(req, mode)
	if err != nil {
		return err
	}
	sender := newCompactEventSender(s.log, stream)
	if err := sender.SendStatus(ctx, "loading transcript and probing context..."); err != nil {
		return err
	}

	upfront, staticOverhead, slice, upfrontErr := compactengine.BuildRuntimeUpfront(ctx, compactengine.RuntimeRequest{
		Session:                run.session,
		Store:                  run.store,
		TargetTokens:           int(req.GetTargetTokens()),
		Reserved:               int(req.GetReservedTokens()),
		Model:                  run.modelForCount,
		ModelExplicit:          false,
		Strippers:              run.strippers,
		Summarize:              false,
		SummarizeMode:          "",
		Force:                  false,
		Mode:                   compactengine.RuntimeModePreview,
		Refresh:                req.GetRefresh(),
		PreparedUpfront:        nil,
		PreparedStaticOverhead: 0,
		PreparedSlice:          nil,
	}, run.modelForRender)
	if upfrontErr != nil {
		return status.Errorf(codes.Internal, "build compact upfront: %v", upfrontErr)
	}
	if err := sender.SendStatus(ctx, "planning compaction..."); err != nil {
		return err
	}
	if err := sender.SendUpfront(ctx, upfront); err != nil {
		return err
	}

	summarizeMode, modeErr := compactengine.NormalizeSummarizeMode(req.GetSummarizeMode())
	if modeErr != nil {
		return status.Errorf(codes.InvalidArgument, "compact summarize mode: %v", modeErr)
	}
	if req.GetSummarizeMode() == "" {
		summarizeMode = compactengine.SummarizeModeFromLegacy(req.GetSummarize(), req.GetSummarize())
	}
	result, runErr := compactengine.RunRuntime(ctx, compactengine.RuntimeRequest{
		Session:                run.session,
		Store:                  run.store,
		TargetTokens:           int(req.GetTargetTokens()),
		Reserved:               int(req.GetReservedTokens()),
		Model:                  run.modelForCount,
		ModelExplicit:          req.GetModelExplicit(),
		Strippers:              run.strippers,
		Summarize:              req.GetSummarize(),
		SummarizeMode:          summarizeMode,
		Force:                  req.GetForce(),
		Mode:                   mode,
		Refresh:                req.GetRefresh(),
		PreparedUpfront:        &upfront,
		PreparedStaticOverhead: staticOverhead,
		PreparedSlice:          slice,
	}, sender.IterationCallback(ctx))
	if runErr != nil {
		return s.compactRunError(ctx, req, runErr)
	}

	if err := sender.SendResult(ctx, result); err != nil {
		return err
	}
	s.log.InfoContext(ctx, "daemon.compact.run_completed",
		"component", "daemon",
		"subcomponent", "compact",
		"session", req.GetSessionName(),
		"session_id", run.session.Metadata.ProviderSessionID(),
		"mode", mode,
		"hit_target", result.Plan.HitTarget,
		"final_tail", result.Plan.FinalTail,
	)
	return nil
}

func (s *Server) compactRunError(
	ctx context.Context,
	req *clydev1.CompactRunRequest,
	runErr error,
) error {
	s.log.ErrorContext(ctx, "daemon.compact.run_failed",
		"component", "daemon",
		"subcomponent", "compact",
		"session", req.GetSessionName(),
		"err", runErr.Error(),
	)
	return status.Errorf(codes.Internal, "compact runtime: %v", runErr)
}

type compactRunSetup struct {
	store          *session.FileStore
	session        *session.Session
	strippers      compactengine.Strippers
	modelForCount  string
	modelForRender string
}

func emptyCompactRunSetup() compactRunSetup {
	return compactRunSetup{
		store:          nil,
		session:        nil,
		strippers:      compactengine.Strippers{Thinking: false, Images: false, Tools: false, Chat: false},
		modelForCount:  "",
		modelForRender: "",
	}
}

func (s *Server) prepareCompactRun(
	req *clydev1.CompactRunRequest,
	mode compactengine.RuntimeMode,
) (compactRunSetup, error) {
	if req.GetSessionName() == "" {
		return emptyCompactRunSetup(), status.Error(codes.InvalidArgument, "session_name is required")
	}
	if req.GetTargetTokens() <= 0 {
		return emptyCompactRunSetup(), status.Error(codes.InvalidArgument, "target_tokens must be greater than zero")
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return emptyCompactRunSetup(), status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return emptyCompactRunSetup(), status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return emptyCompactRunSetup(), status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	if !sess.SessionProviderCapabilities().Compaction {
		return emptyCompactRunSetup(), status.Errorf(codes.FailedPrecondition, "session provider %q does not support compaction", sess.ProviderID())
	}
	if mode == compactengine.RuntimeModeApply && !req.GetForce() && s.sessionIsActive(sess.Name) {
		return emptyCompactRunSetup(), status.Errorf(codes.FailedPrecondition, "session %q is currently open; exit it first or pass --force", sess.Name)
	}
	modelForCount := req.GetModel()
	modelForRender := req.GetModel()
	if !req.GetModelExplicit() {
		modelForCount, modelForRender, _ = compactengine.ResolveModelForCounting(store, sess, req.GetModel())
	}
	return compactRunSetup{
		store:          store,
		session:        sess,
		strippers:      compactRunStrippers(req),
		modelForCount:  modelForCount,
		modelForRender: modelForRender,
	}, nil
}

func compactRunStrippers(req *clydev1.CompactRunRequest) compactengine.Strippers {
	strippers := compactengine.Strippers{
		Thinking: false,
		Images:   false,
		Tools:    false,
		Chat:     false,
	}
	if req.GetStrippers() != nil {
		strippers = compactengine.Strippers{
			Thinking: req.GetStrippers().GetThinking(),
			Images:   req.GetStrippers().GetImages(),
			Tools:    req.GetStrippers().GetTools(),
			Chat:     req.GetStrippers().GetChat(),
		}
	}
	if !strippers.Any() && req.GetTargetTokens() > 0 {
		strippers.SetAll()
	}
	return strippers
}

type compactEventSender struct {
	log      *slog.Logger
	sequence int32
	stream   compactEventStream
}

func newCompactEventSender(log *slog.Logger, stream compactEventStream) *compactEventSender {
	return &compactEventSender{
		log:      log,
		sequence: 0,
		stream:   stream,
	}
}

func (s *compactEventSender) Send(ctx context.Context, ev *clydev1.CompactEvent) error {
	s.sequence++
	ev.Sequence = s.sequence
	if err := s.stream.Send(ev); err != nil {
		s.log.WarnContext(ctx, "daemon.compact.event_send_failed",
			"component", "daemon",
			"subcomponent", "compact",
			"kind", ev.GetKind(),
			"sequence", ev.GetSequence(),
			"err", err,
		)
		return fmt.Errorf("send compact event: %w", err)
	}
	return nil
}

func (s *compactEventSender) SendStatus(ctx context.Context, message string) error {
	return s.Send(ctx, &clydev1.CompactEvent{
		Kind:    clydev1.CompactEvent_KIND_STATUS,
		Message: message,
	})
}

func (s *compactEventSender) SendUpfront(ctx context.Context, upfront compactengine.RuntimeUpfront) error {
	return s.Send(ctx, &clydev1.CompactEvent{
		Kind:    clydev1.CompactEvent_KIND_UPFRONT,
		Upfront: compactUpfrontProto(upfront),
	})
}

func (s *compactEventSender) IterationCallback(ctx context.Context) func(compactengine.RuntimeIteration) {
	return func(iter compactengine.RuntimeIteration) {
		_ = s.Send(ctx, &clydev1.CompactEvent{
			Kind:      clydev1.CompactEvent_KIND_ITERATION,
			Iteration: compactIterationProto(s.sequence, iter),
		})
	}
}

func (s *compactEventSender) SendResult(ctx context.Context, result *compactengine.RuntimeResult) error {
	if err := s.Send(ctx, &clydev1.CompactEvent{
		Kind:  clydev1.CompactEvent_KIND_FINAL,
		Final: compactFinalProto(result),
	}); err != nil {
		return err
	}
	if result.Apply == nil {
		return nil
	}
	return s.Send(ctx, &clydev1.CompactEvent{
		Kind:          clydev1.CompactEvent_KIND_APPLY_MUTATION,
		ApplyMutation: compactApplyMutationProto(result.Apply),
	})
}

func compactUpfrontProto(upfront compactengine.RuntimeUpfront) *clydev1.CompactUpfront {
	categories := make([]*clydev1.CompactUsageCategory, 0, len(upfront.UsageCategories))
	for _, cat := range upfront.UsageCategories {
		categories = append(categories, &clydev1.CompactUsageCategory{
			Name:       cat.Name,
			Tokens:     compactInt32(cat.Tokens),
			IsDeferred: cat.IsDeferred,
		})
	}
	boundaryTime := ""
	if upfront.HasBoundary {
		boundaryTime = upfront.BoundaryTime.UTC().Format(time.RFC3339)
	}
	usageCapturedAt := ""
	if upfront.UsageAvailable {
		usageCapturedAt = upfront.UsageCapturedAt.UTC().Format(time.RFC3339)
	}
	return &clydev1.CompactUpfront{
		SessionName:           upfront.SessionName,
		SessionId:             upfront.SessionID,
		Model:                 upfront.Model,
		CurrentTotal:          compactInt32(upfront.CurrentTotal),
		MaxTokens:             compactInt32(upfront.MaxTokens),
		TargetTokens:          compactInt32(upfront.Target),
		StaticFloor:           compactInt32(upfront.StaticFloor),
		ReservedTokens:        compactInt32(upfront.Reserved),
		ThinkingBlocks:        compactInt32(upfront.Thinking),
		ImageBlocks:           compactInt32(upfront.Images),
		ToolPairs:             compactInt32(upfront.ToolPairs),
		ChatTurns:             compactInt32(upfront.ChatTurns),
		StrippersText:         upfront.StrippersText,
		CalibrationDate:       upfront.TargetDate,
		MessagesTokens:        compactInt32(upfront.Messages),
		CompactBufferTokens:   compactInt32(upfront.CompactBuffer),
		FreeTokens:            compactInt32(upfront.Free),
		ContextOverheadTokens: compactInt32(upfront.ContextOverhead),
		PostBoundaryEntries:   compactInt32(upfront.PostBoundaryEntries),
		Calibrated:            upfront.Calibrated,
		CalibrationOverhead:   compactInt32(upfront.CalibrationOverhead),
		TranscriptPath:        upfront.TranscriptPath,
		FileSizeBytes:         upfront.FileSizeBytes,
		FileLineCount:         compactInt32(upfront.FileLineCount),
		HasBoundary:           upfront.HasBoundary,
		BoundaryLine:          compactInt32(upfront.BoundaryLine),
		BoundaryUuid:          upfront.BoundaryUUID,
		BoundaryTime:          boundaryTime,
		UsagePercentage:       compactInt32(upfront.UsagePercentage),
		UsageAvailable:        upfront.UsageAvailable,
		UsageSource:           upfront.UsageSource,
		UsageCapturedAt:       usageCapturedAt,
		UsageError:            upfront.UsageError,
		UsageCategories:       categories,
	}
}

func compactIterationProto(sequence int32, iter compactengine.RuntimeIteration) *clydev1.CompactIteration {
	return &clydev1.CompactIteration{
		Iteration:         sequence,
		Step:              iter.Iteration.Step,
		TailTokens:        compactInt32(iter.Iteration.TailTokens),
		CtxTotal:          compactInt32(iter.Iteration.CtxTotal),
		Delta:             compactInt32(iter.Iteration.Delta),
		ThinkingDropped:   iter.Iteration.ThinkingDropped,
		ImagesPlaceholder: iter.Iteration.ImagesPlaceholder,
		ToolsFull:         compactInt32(iter.Iteration.ToolsFull),
		ToolsLineOnly:     compactInt32(iter.Iteration.ToolsLineOnly),
		ToolsDropped:      compactInt32(iter.Iteration.ToolsDropped),
		ChatTurnsTotal:    compactInt32(iter.Iteration.ChatTurnsTotal),
		ChatTurnsDropped:  compactInt32(iter.Iteration.ChatTurnsDropped),
		Probe:             iter.Iteration.Probe,
	}
}

func compactFinalProto(result *compactengine.RuntimeResult) *clydev1.CompactFinal {
	return &clydev1.CompactFinal{
		BaselineTail:       compactInt32(result.Plan.BaselineTail),
		FinalTail:          compactInt32(result.Plan.FinalTail),
		HitTarget:          result.Plan.HitTarget,
		TargetTokens:       compactInt32(result.Upfront.Target),
		StaticFloor:        compactInt32(result.Upfront.StaticFloor),
		ReservedTokens:     compactInt32(result.Upfront.Reserved),
		TranscriptPath:     result.TranscriptPath,
		BoundaryTailBlocks: compactInt32(len(result.Plan.BoundaryTail)),
	}
}

func compactApplyMutationProto(applyResult *compactengine.ApplyResult) *clydev1.CompactApplyMutation {
	return &clydev1.CompactApplyMutation{
		BoundaryUuid:    applyResult.BoundaryUUID,
		SyntheticUuid:   applyResult.SyntheticUUID,
		PreApplyOffset:  applyResult.PreApplyOffset,
		PostApplyOffset: applyResult.PostApplyOffset,
		SnapshotPath:    applyResult.SnapshotPath,
		LedgerPath:      applyResult.LedgerPath,
	}
}

func compactInt32(value int) int32 {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	if value < math.MinInt32 {
		return math.MinInt32
	}
	return int32(value)
}

// CompactUndo restores the most recent compact snapshot for one session.
func (s *Server) CompactUndo(ctx context.Context, req *clydev1.CompactUndoRequest) (*clydev1.CompactUndoResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	incomingMD, _ := metadata.FromIncomingContext(ctx)
	if req.GetSessionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "session_name is required")
	}
	s.log.InfoContext(ctx, "daemon.compact.undo.started",
		"component", "daemon",
		"subcomponent", "compact",
		"session", req.GetSessionName(),
		"peer_addr", daemonPeerAddr(peerInfo),
		"metadata_keys", daemonMetadataKeys(incomingMD),
	)
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(req.GetSessionName())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session: %v", err)
	}
	if sess == nil {
		return nil, status.Errorf(codes.NotFound, "session %q not found", req.GetSessionName())
	}
	path := sess.Metadata.ProviderTranscriptPath()
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "session has no transcript")
	}
	entry, undoErr := compactengine.Undo(sess.Metadata.ProviderSessionID(), path)
	if undoErr != nil {
		s.log.ErrorContext(ctx, "daemon.compact.undo_failed",
			"component", "daemon",
			"subcomponent", "compact",
			"session", req.GetSessionName(),
			"session_id", sess.Metadata.ProviderSessionID(),
			"err", undoErr.Error(),
		)
		return nil, status.Errorf(codes.Internal, "undo: %v", undoErr)
	}
	ledgerPath, _ := compactengine.LedgerPath(sess.Metadata.ProviderSessionID())
	postBytes := int64(-1)
	if stat, statErr := os.Stat(path); statErr == nil {
		postBytes = stat.Size()
	}
	resp := &clydev1.CompactUndoResponse{
		SessionName:    sess.Name,
		SessionId:      sess.Metadata.ProviderSessionID(),
		TranscriptPath: path,
		LedgerPath:     ledgerPath,
		AppliedAt:      entry.Timestamp.UTC().Format(time.RFC3339),
		TargetTokens:   compactInt32(entry.Target),
		BoundaryUuid:   entry.BoundaryUUID,
		SyntheticUuid:  entry.SyntheticUUID,
		SnapshotPath:   entry.SnapshotPath,
		PreApplyOffset: entry.PreApplyOffset,
		PostUndoBytes:  postBytes,
	}
	s.log.InfoContext(ctx, "daemon.compact.undo_completed",
		"component", "daemon",
		"subcomponent", "compact",
		"session", req.GetSessionName(),
		"session_id", sess.Metadata.ProviderSessionID(),
		"pre_apply_offset", entry.PreApplyOffset,
		"post_undo_bytes", postBytes,
	)
	return resp, nil
}

type compactEventStream interface {
	Send(*clydev1.CompactEvent) error
}

// injectSocketPath returns the per session Unix socket path the
// wrapper opens when launched with --remote-control. The directory is
// created lazily by the wrapper.
func injectSocketPath(sessionID string) string {
	dir := injectSocketDir()
	return filepath.Join(dir, sessionID+".sock")
}

// injectSocketDir is the per user directory that holds inject sockets.
// Lives under XDG_RUNTIME_DIR when set, otherwise the per user temp
// directory so macOS users (no XDG_RUNTIME_DIR by default) still get a
// session local path.
func injectSocketDir() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "clyde", "inject")
	}
	return filepath.Join(os.TempDir(), "clyde-inject")
}
