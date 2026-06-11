package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/slogger"
)

// configReloadDebounce is the quiet period after the last file event before the
// watcher acts. Editors emit a burst of write/rename/chmod events when saving;
// the debounce collapses the burst into one reload.
const configReloadDebounce = 200 * time.Millisecond

// configAbsentHash is the sentinel hash for a missing config file. Deleting the
// config is a change to defaults, so it must differ from any real file hash and
// from the startup baseline of an existing file.
const configAbsentHash = "absent"

// configWatcherMeta is the livetrack metadata for the config-watcher session.
type configWatcherMeta struct{ Path string }

// IsLivetrackMeta marks configWatcherMeta as livetrack session metadata.
func (configWatcherMeta) IsLivetrackMeta() {}

// configWatcherCloser cancels the watcher loop when livetrack drains the
// registry on shutdown or reload.
type configWatcherCloser struct{ cancel context.CancelFunc }

// Close cancels the watcher loop context. It satisfies livetrack.Closer.
func (c *configWatcherCloser) Close(string) error {
	c.cancel()
	return nil
}

// configWatcher watches the config file's directory and triggers a daemon
// reload when the file's content changes. It mirrors the agent-gate watcher:
// watch the directory (not the file, which an editor's rename-over would
// orphan), filter to the config path, and debounce.
type configWatcher struct {
	path         string
	log          *slog.Logger
	debounce     time.Duration
	baselineHash string
	registry     *livetrack.Registry[configWatcherMeta]
	// cancel stops the loop goroutine. It is set in start and called by stop
	// so the loop exits and releases its livetrack session promptly, rather
	// than waiting for the drain deadline to force-close the closer.
	cancel context.CancelFunc

	// parse validates the candidate config before triggering. Defaults to
	// config.LoadGlobalOrDefault; injected in tests.
	parse func() (*config.Config, error)
	// reload triggers the zero-bind-gap reload. Defaults to ReloadDaemon;
	// injected in tests.
	reload func(context.Context) error
	// rebind triggers the fresh-bind replacement. Defaults to a
	// restart-required error until the supervisor rebind path is wired;
	// injected in tests.
	rebind func(context.Context) error
	// quietWait blocks until no client exchange is in flight, bounded by the
	// reload quiet-wait, so a reload or rebind defers to in-flight requests
	// instead of severing them. Defaults to awaitDaemonQuiet over the daemon
	// lifecycle group; injected in tests. Returns true if quiet was reached.
	quietWait func(context.Context) bool
	// classify decides whether a parsed config change hot-applies, reloads,
	// rebinds, or needs a restart, comparing against the running config.
	// Defaults to config.ClassifyConfigChange over the runtime's current
	// config; injected in tests.
	classify func(next *config.Config) config.Route
	// applyInProcess swaps the hot-appliable config-derived state without
	// re-exec. Defaults to the runtime's applyConfigInProcess; injected in
	// tests. A nil applyInProcess makes a hot-apply route fall through to
	// reload.
	applyInProcess func(context.Context, *config.Config) error
}

// newConfigWatcher builds the production watcher. The reload trigger is the
// ReloadDaemon client, which takes the daemon.reload.lock flock and so
// serializes against CLI-initiated reloads. The listener comparison reuses
// validateReloadListenerConfig, which reloads config and diffs it against the
// running listeners.
func newConfigWatcher(log *slog.Logger, baselineHash string, runtime *runtimeServices) *configWatcher {
	w := &configWatcher{
		path:         config.GlobalConfigPath(),
		log:          log,
		debounce:     configReloadDebounce,
		baselineHash: baselineHash,
		cancel:       nil,
		registry: livetrack.Attach[configWatcherMeta](runtime.group, livetrack.MemberSpec{
			Phase:         livetrack.PhaseWorkers,
			QuietRelevant: false,
			CancelNoWait:  true,
		}, livetrack.Options[configWatcherMeta]{
			Component:     "daemon",
			Concern:       slogger.ConcernProcessDaemonConfig,
			Log:           log,
			PollEvery:     0,
			CloserGrace:   0,
			ParallelClose: false,
			Now:           nil,
		}),
		parse: config.LoadGlobalOrDefault,
		reload: func(ctx context.Context) error {
			_, err := ReloadDaemon(ctx)
			return err
		},
		rebind: func(ctx context.Context) error {
			_, err := RebindDaemon(ctx)
			return err
		},
		quietWait: func(ctx context.Context) bool {
			return awaitDaemonQuiet(ctx, runtime.group)
		},
		classify: func(next *config.Config) config.Route {
			current := runtime.currentConfig.Load()
			if current == nil || next == nil {
				return config.RouteReload
			}
			return config.ClassifyConfigChange(*current, *next)
		},
		applyInProcess: func(ctx context.Context, next *config.Config) error {
			return runtime.applyConfigInProcess(ctx, log, next)
		},
	}
	// configWatcherMeta.IsLivetrackMeta is a pure marker the registry never
	// calls. Anchor one dynamic dispatch to it from this reachable
	// constructor so the deadcode gate sees the constraint method as live.
	var marker livetrack.Meta = configWatcherMeta{Path: ""}
	marker.IsLivetrackMeta()
	return w
}

// start opens the fsnotify watcher on the config directory, registers a
// livetrack session, and runs the poll loop in a recovered goroutine. It
// returns an error only when the watcher cannot be created or registered; a
// failure here is non-fatal to the daemon, so the caller logs and continues.
func (w *configWatcher) start(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create config watcher: %w", err)
	}
	dir := filepath.Dir(w.path)
	if err := fsw.Add(dir); err != nil {
		_ = fsw.Close()
		return fmt.Errorf("watch config dir %s: %w", dir, err)
	}
	loopCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	sess, err := w.registry.Register(loopCtx, "config.watch", configWatcherMeta{Path: w.path}, &configWatcherCloser{cancel: cancel})
	if err != nil {
		cancel()
		_ = fsw.Close()
		return fmt.Errorf("register config watcher: %w", err)
	}
	w.log.InfoContext(ctx, "daemon.config_watch.started", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
		"path", w.path,
		"debounce", w.debounce.String(),
	)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				w.log.ErrorContext(loopCtx, "daemon.config_watch.panic", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		defer w.registry.Release(loopCtx, sess, "config.watch.done")
		defer func() { _ = fsw.Close() }()
		w.loop(loopCtx, fsw)
	}()
	return nil
}

// cancelLoop cancels the watcher loop without waiting. It is nil-safe and
// non-blocking, so a reload triggered from inside the loop can stop the loop
// without deadlocking on the loop's own in-flight reload RPC. The loop releases
// its livetrack session when it returns.
func (w *configWatcher) cancelLoop() {
	if w == nil || w.cancel == nil {
		return
	}
	w.cancel()
}

// loop is the select over fsnotify events, errors, and the debounce timer. A
// matching event arms the timer; the timer firing with a pending change does
// the work. It returns when the context is cancelled, the watcher closes, or a
// reload is triggered (the process is then being replaced).
func (w *configWatcher) loop(ctx context.Context, fsw *fsnotify.Watcher) {
	timer := time.NewTimer(w.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer func() { _ = timer.Stop() }()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fsw.Events:
			if !ok {
				return
			}
			if w.matches(event) {
				pending = true
				resetConfigReloadTimer(timer, w.debounce)
			}
		case <-timer.C:
			if !pending {
				continue
			}
			pending = false
			if w.handleChange(ctx) {
				return
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			w.log.WarnContext(ctx, "daemon.config_watch.error", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
				"err", err,
			)
		}
	}
}

// matches reports whether an fsnotify event is a content-affecting event on the
// config file itself. Events for sibling files in the directory (for example a
// config.toml.bak.* backup) are filtered out by the path comparison.
func (w *configWatcher) matches(event fsnotify.Event) bool {
	if filepath.Clean(event.Name) != filepath.Clean(w.path) {
		return false
	}
	const reloadEvents = fsnotify.Write | fsnotify.Create | fsnotify.Rename | fsnotify.Remove | fsnotify.Chmod
	return event.Op&reloadEvents != 0
}

// handleChange runs after the debounce settles. It returns true when a reload
// was triggered and the loop should exit because this process is being
// replaced; it returns false to keep watching.
func (w *configWatcher) handleChange(ctx context.Context) bool {
	hash, ok := configFileHash(ctx, w.log, w.path)
	if !ok {
		return false
	}
	if hash == w.baselineHash {
		return false
	}
	next, err := w.parse()
	if err != nil {
		w.log.WarnContext(ctx, "daemon.config_watch.parse_failed", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
			"path", w.path,
			"err", err,
		)
		return false
	}
	w.log.InfoContext(ctx, "daemon.config_watch.change_detected", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
		"path", w.path,
	)
	// Classify the change. A restart-required change cannot be honored by the
	// running generation, so log and keep watching rather than re-exec into a
	// config it cannot serve. A hot-appliable change is swapped in process with
	// no re-exec and no severed exchanges; on apply failure it falls through to
	// the reload path below.
	route := config.RouteReload
	if w.classify != nil {
		route = w.classify(next)
	}
	w.log.InfoContext(ctx, "daemon.config_watch.classified", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
		"path", w.path,
		"route", route.String(),
	)
	if route == config.RouteRestartRequired {
		w.log.WarnContext(ctx, "daemon.config_watch.restart_required", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
			"path", w.path,
		)
		return false
	}
	if route == config.RouteHotApply && w.applyInProcess != nil {
		if applyErr := w.applyInProcess(ctx, next); applyErr != nil {
			w.log.WarnContext(ctx, "daemon.config_watch.hot_apply_failed", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
				"path", w.path,
				"err", applyErr,
			)
		} else {
			w.baselineHash = hash
			return false
		}
	}
	// Wait for in-flight client exchanges to finish before triggering a reload
	// or rebind that would sever them, bounded by the reload quiet-wait. A
	// request longer than the bound still takes the drain.
	if w.quietWait != nil && !w.quietWait(ctx) {
		w.log.InfoContext(ctx, "daemon.config_watch.quiet_wait_timeout", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
			"path", w.path,
		)
	}
	// Re-hash after the wait: an edit reverted to the baseline while we waited
	// means there is nothing to apply, so skip the reload entirely.
	postHash, postOK := configFileHash(ctx, w.log, w.path)
	if !postOK || postHash == w.baselineHash {
		w.log.InfoContext(ctx, "daemon.config_watch.reverted_during_wait", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
			"path", w.path,
		)
		return false
	}
	// Route reload vs rebind on the single classifier decision: a listener move
	// (RouteRebind) frees and rebinds the ports fresh, while every other
	// reload-class change re-execs inheriting the open sockets. A hot apply that
	// fell through here also takes the reload path.
	if route == config.RouteRebind {
		return w.trigger(ctx, "rebind", w.rebind)
	}
	return w.trigger(ctx, "reload", w.reload)
}

// trigger runs one reload or rebind and logs the outcome. It returns true only
// on success, when the loop should exit because the process is being replaced.
//
// The trigger call uses a context detached from the loop's cancellation:
// reloadDaemonWorker stops this watcher once the replacement is ready, which
// cancels the loop context. If the self-RPC rode that same context it would be
// cancelled mid-flight and report a spurious rejection even though the reload
// succeeded server-side. The client carries its own overall timeout.
func (w *configWatcher) trigger(parent context.Context, kind string, fn func(context.Context) error) bool {
	ctx := context.WithoutCancel(parent)
	if err := fn(ctx); err != nil {
		w.log.WarnContext(ctx, "daemon.config_watch.reload_rejected", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
			"kind", kind,
			"err", err,
		)
		return false
	}
	w.log.InfoContext(ctx, "daemon.config_watch.reload_triggered", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
		"kind", kind,
	)
	return true
}

// resetConfigReloadTimer stops and re-arms the debounce timer, draining the
// channel if the timer had already fired. It mirrors agent-gate's resetTimer.
func resetConfigReloadTimer(timer *time.Timer, duration time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(duration)
}

// configFileHash returns the sha256 of the config file's contents and true, or
// the absent sentinel and true when the file does not exist. A hard read error
// is logged here (adjacent to the os call) and returns ok=false, so no wrapped
// error crosses a function boundary unlogged.
func configFileHash(ctx context.Context, log *slog.Logger, path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return configAbsentHash, true
		}
		log.WarnContext(ctx, "daemon.config_watch.hash_failed", "concern", slogger.ConcernProcessDaemonConfig, "component", "daemon",
			"path", path,
			"err", err,
		)
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
}
