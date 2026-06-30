package daemon

import (
	"context"
	"log/slog"
	"os"
	"time"

	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/mitm/capture"
)

// budgetReload and budgetShutdown replace the scattered reloadDrainCap,
// reloadDrainIdleGrace, and daemonShutdownTimeout constants. The values are
// identical so Quiesce drains on the same schedule the hand-sequenced path
// used.
var (
	budgetReload   = livetrack.Budget{Cap: 60 * time.Second, IdleGrace: 5 * time.Second}
	budgetShutdown = livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0}
)

// defaultReloadQuietWait bounds the watcher's pre-reload quiet wait. A reload
// defers at most this long for in-flight client exchanges before proceeding to
// the drain. Held next to the budgets so all reload timing lives in one file.
const defaultReloadQuietWait = 30 * time.Second

// reloadQuietWait is the active quiet-wait bound. It is the default unless the
// CLYDE_RELOAD_QUIET_WAIT env var sets a shorter (or longer) Go duration, which
// the live validation suite uses to exercise the cap-fires path without a 30s
// wait. Invalid or empty values keep the default.
var reloadQuietWait = resolveReloadQuietWait()

func resolveReloadQuietWait() time.Duration {
	if raw := os.Getenv("CLYDE_RELOAD_QUIET_WAIT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultReloadQuietWait
}

// newLifecycleGroup constructs the daemon's lifecycle group. Subsystems Attach
// their registries to it at construction and AddHook their non-registry steps;
// the daemon never hand-sequences drain order again.
func newLifecycleGroup(log *slog.Logger) *livetrack.Group {
	return livetrack.NewGroup(livetrack.GroupOptions{Log: log})
}

// awaitDaemonQuiet waits until no client-exchange surface has an active session
// within budgetReload.IdleGrace, bounded by reloadQuietWait. The watcher calls
// it before triggering reload or rebind.
func awaitDaemonQuiet(ctx context.Context, group *livetrack.Group) bool {
	if group == nil {
		return true
	}
	waitCtx, cancel := context.WithTimeout(ctx, reloadQuietWait)
	defer cancel()
	return group.AwaitQuiet(waitCtx, budgetReload.IdleGrace)
}

// storageHookReason labels the lifecycle store-close hooks. The drain reason
// (reload vs shutdown) is not threaded into hooks, so a single stable label is
// used; the registry and store emit their own per-event detail.
const storageHookReason = "lifecycle.drain"

// addAdapterLifecycleHooks registers the adapter's non-registry drain steps on
// the group. Keepalives-off runs before the ingress registry drains; the HTTP
// server shutdown runs after ingress drains but before egress, reproducing the
// pre-refactor ShutdownWith ordering. The ingress, egress, and codex websocket
// registries drain as group members.
func (r *runtimeServices) addAdapterLifecycleHooks(server *adapter.Server) {
	if r.group == nil || server == nil {
		return
	}
	r.group.AddHookBefore(livetrack.PhaseIngress, "adapter.keepalives_off", func(context.Context) error {
		server.DisableKeepAlives()
		return nil
	})
	r.group.AddHookBefore(livetrack.PhaseEgress, "adapter.http_shutdown", func(ctx context.Context) error {
		return server.ShutdownHTTP(ctx)
	})
}

// addMITMLifecycleHook registers one MITM proxy's HTTP server shutdown as a
// PhaseIngress before-hook so the server stops accepting before its tunnel
// registry (a group member) drains.
func (r *runtimeServices) addMITMLifecycleHook(id string, proxy *mitm.Proxy) {
	if r.group == nil || proxy == nil {
		return
	}
	r.group.AddHookBefore(livetrack.PhaseIngress, "mitm.http_shutdown."+id, func(ctx context.Context) error {
		return proxy.ShutdownHTTP(ctx)
	})
}

// addCaptureStoreCloseHook registers the MITM capture store's close as a
// PhaseStorage after-hook so its SQLite WAL flushes only after every surface
// has drained.
func (r *runtimeServices) addCaptureStoreCloseHook(store *capture.Store) {
	if r.group == nil || store == nil {
		return
	}
	r.group.AddHook(livetrack.PhaseStorage, "capture_store.close", func(ctx context.Context) error {
		return store.Close(ctx, storageHookReason)
	})
}
