package sandbox

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// orphanInitPID is the parent a process inherits when whatever started it exits.
const orphanInitPID = 1

// orphanPollInterval is how often the guard checks its own parentage. The call
// is a cheap syscall, and a sandbox holding a lock or a collection is worth
// noticing quickly.
const orphanPollInterval = 2 * time.Second

// WatchParent cancels ctx once this process is reparented to init, meaning
// whatever launched it is gone.
//
// A sandbox stops on Ctrl-C, on a kill, and when its terminal closes, because
// each of those signals it. A launcher that dies without signalling leaves
// nothing to notice, and the daemon would keep holding its roots and its
// collection with nobody left to stop it. This closes that case.
//
// It asks only about its own parentage and never tracks another process by
// identifier, so a recycled identifier cannot make it fire or miss. It returns
// when ctx is cancelled or once it has fired.
func WatchParent(ctx context.Context, cancel context.CancelFunc) {
	if os.Getppid() == orphanInitPID {
		// Already parentless, so there is no exit to wait for. A daemon started
		// directly by init or a service manager is meant to keep running.
		return
	}
	ticker := time.NewTicker(orphanPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if os.Getppid() != orphanInitPID {
				continue
			}
			slog.WarnContext(ctx, "sandbox.parent.exited",
				"concern", "process.daemon.lifecycle",
				"component", "sandbox",
			)
			cancel()
			return
		}
	}
}
