// Package properlock implements the directory-lock format produced by the
// proper-lockfile npm package. Claude Code uses proper-lockfile to serialize
// OAuth refreshes at the Claude config directory, so Clyde must use the same
// format when it wants to coordinate with a running Claude Code process. The
// lock is a directory named "<target>.lock" placed next to the target path.
// Acquire calls [os.Mkdir], which is atomic on POSIX; a stale lock dir whose
// mtime is older than [Options.StaleThreshold] is treated as abandoned and
// removed before the next retry. A background goroutine refreshes the mtime
// while the lock is held so concurrent readers do not see the lock as
// stale.
package properlock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Default values mirror the proper-lockfile npm package defaults. The library
// uses 10s for stale detection, a small retry wait, and refreshes the mtime
// every 5s while the lock is held. The 30s acquire timeout matches the
// behavior Claude Code uses for the OAuth refresh path.
const (
	defaultStaleThreshold       = 10 * time.Second
	defaultAcquireTimeout       = 30 * time.Second
	defaultRetryWait            = 100 * time.Millisecond
	defaultMtimeRefreshInterval = 5 * time.Second
	lockDirMode                 = 0o700
)

// ErrTimeout is returned by Acquire when the lock could not be obtained
// within Options.AcquireTimeout. Callers can detect it with [errors.Is].
var ErrTimeout = errors.New("properlock: acquire timed out")

// Options tunes the proper-lockfile-compatible Acquire behavior. A zero value
// in a duration field falls back to the package default. Now is the time
// source used for the staleness comparison; callers inject one from their
// own time-access boundary so this package never calls [time.Now] directly.
// A nil Now falls back to [time.Now].
type Options struct {
	// StaleThreshold is how long a lock dir's mtime may lag wall-clock before
	// the next acquire considers it abandoned and removes it.
	StaleThreshold time.Duration
	// AcquireTimeout bounds how long Acquire waits for a contended lock.
	AcquireTimeout time.Duration
	// RetryWait is the sleep between acquire attempts.
	RetryWait time.Duration
	// MtimeRefreshInterval is the period at which the holder refreshes the
	// lock dir's mtime so concurrent readers do not see it as stale.
	MtimeRefreshInterval time.Duration
	// Now returns the current wall-clock time. The package never calls
	// time.Now directly so the caller's clock-injection layer is the single
	// authority. A nil value falls back to the stdlib.
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	out := o
	if out.StaleThreshold <= 0 {
		out.StaleThreshold = defaultStaleThreshold
	}
	if out.AcquireTimeout <= 0 {
		out.AcquireTimeout = defaultAcquireTimeout
	}
	if out.RetryWait <= 0 {
		out.RetryWait = defaultRetryWait
	}
	if out.MtimeRefreshInterval <= 0 {
		out.MtimeRefreshInterval = defaultMtimeRefreshInterval
	}
	if out.Now == nil {
		out.Now = time.Now
	}
	return out
}

// LockPath returns the on-disk lock directory path for a target. Exposed so
// tests and the migration path can reference the same convention.
func LockPath(targetPath string) string {
	return targetPath + ".lock"
}

// Acquire takes the proper-lockfile directory lock for targetPath and
// returns a release function. The release function stops the mtime refresh
// goroutine and removes the lock directory; it is safe to call more than
// once. Context cancellation aborts the wait loop and returns ctx.Err.
//
// Acquire derives its deadline by wrapping ctx in a context with the
// configured acquire timeout, so a contended lock with no progress fails
// fast even when the caller's context has no deadline of its own. The
// underlying mkdir call is atomic on POSIX, so an ErrExist response means
// some other process holds the lock.
func Acquire(ctx context.Context, targetPath string, opts Options) (func() error, error) {
	opts = opts.withDefaults()
	lockPath := LockPath(targetPath)

	acquireCtx, cancel := context.WithTimeout(ctx, opts.AcquireTimeout)
	defer cancel()

	for {
		err := os.Mkdir(lockPath, lockDirMode)
		if err == nil {
			slog.DebugContext(ctx, "properlock.acquired",
				"component", "properlock",
				"lock_path", lockPath,
			)
			return startHolder(lockPath, opts), nil
		}
		if !errors.Is(err, os.ErrExist) {
			slog.WarnContext(ctx, "properlock.mkdir_failed",
				"component", "properlock",
				"lock_path", lockPath,
				"err", err.Error(),
			)
			return nil, fmt.Errorf("properlock: mkdir %s: %w", lockPath, err)
		}

		stale, statErr := readLockMtime(lockPath, opts.StaleThreshold, opts.Now)
		if statErr != nil {
			slog.WarnContext(ctx, "properlock.stat_failed",
				"component", "properlock",
				"lock_path", lockPath,
				"err", statErr.Error(),
			)
			return nil, fmt.Errorf("properlock: stat %s: %w", lockPath, statErr)
		}
		if stale {
			// Stale: try to steal. Ignore remove errors so a concurrent
			// stealer's success surfaces as the next Mkdir's ErrExist.
			_ = os.Remove(lockPath)
			continue
		}

		select {
		case <-acquireCtx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				slog.WarnContext(ctx, "properlock.acquire_cancelled",
					"component", "properlock",
					"lock_path", lockPath,
					"err", ctx.Err().Error(),
				)
				return nil, fmt.Errorf("properlock: %s: %w", lockPath, ctx.Err())
			}
			slog.WarnContext(ctx, "properlock.acquire_timeout",
				"component", "properlock",
				"lock_path", lockPath,
				"timeout_ms", opts.AcquireTimeout.Milliseconds(),
			)
			return nil, fmt.Errorf("properlock: %s: %w", lockPath, ErrTimeout)
		case <-time.After(opts.RetryWait):
		}
	}
}

// readLockMtime stats the lock directory and reports whether its mtime is
// older than threshold according to the supplied now function. Stat
// returning ErrNotExist is treated as "not stale" so the caller loops back
// to the mkdir attempt instead of trying to remove a path that already
// vanished.
func readLockMtime(lockPath string, threshold time.Duration, now func() time.Time) (bool, error) {
	info, err := os.Stat(lockPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		slog.Warn("properlock.stat_unexpected_failed",
			"component", "properlock",
			"lock_path", lockPath,
			"err", err.Error(),
		)
		return false, fmt.Errorf("properlock: stat %s: %w", lockPath, err)
	}
	return now().Sub(info.ModTime()) > threshold, nil
}

// startHolder spawns the mtime refresh goroutine and returns a release
// function. The goroutine exits when stop is closed; the released flag
// guarantees the lock dir is removed at most once.
func startHolder(lockPath string, opts Options) func() error {
	stop := make(chan struct{})
	var (
		released bool
		mu       sync.Mutex
	)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("properlock.refresh_goroutine_panic",
					"component", "properlock",
					"lock_path", lockPath,
					"err", fmt.Sprintf("%v", r),
				)
			}
		}()
		refreshMtimeLoop(lockPath, opts.MtimeRefreshInterval, stop)
	}()

	return func() error {
		mu.Lock()
		defer mu.Unlock()
		if released {
			return nil
		}
		released = true
		close(stop)
		if err := os.Remove(lockPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			slog.Warn("properlock.release_failed",
				"component", "properlock",
				"lock_path", lockPath,
				"err", err.Error(),
			)
			return fmt.Errorf("properlock: remove %s: %w", lockPath, err)
		}
		return nil
	}
}

// refreshMtimeLoop bumps the lock dir's mtime on each tick so concurrent
// stealers do not classify the lock as stale. A failed Chtimes is silent:
// the next tick retries, and if the directory has vanished the loop simply
// keeps trying until release.
func refreshMtimeLoop(lockPath string, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			_ = os.Chtimes(lockPath, now, now)
		}
	}
}
