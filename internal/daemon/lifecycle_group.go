package daemon

import (
	"context"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/livetrack"
)

// budgetReload and budgetShutdown replace the scattered reloadDrainCap,
// reloadDrainIdleGrace, and daemonShutdownTimeout constants. The values are
// identical so Quiesce drains on the same schedule the hand-sequenced path
// used.
var (
	budgetReload   = livetrack.Budget{Cap: 60 * time.Second, IdleGrace: 5 * time.Second}
	budgetShutdown = livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0}
)

// reloadQuietWait bounds the watcher's pre-reload quiet wait. A reload defers at
// most this long for in-flight client exchanges before proceeding to the drain.
// Held next to the budgets so all reload timing lives in one file.
const reloadQuietWait = 30 * time.Second

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
