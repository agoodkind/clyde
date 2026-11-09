package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"goodkind.io/clyde/internal/config"
)

var errReloadLockPanic = errors.New("reload lock panic")

func lockDaemonReload(ctx context.Context) (func(), error) {
	err := config.EnsureRuntimeDir()
	if err != nil {
		return nil, fmt.Errorf("ensure runtime dir for reload lock: %w", err)
	}

	lockPath := daemonReloadLockPath()

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "daemon.client.reload_lock.open_failed",
			"concern", "process.daemon.lifecycle", "component", "daemon",
			"lock_path", lockPath, "err", err)

		return nil, fmt.Errorf("open reload lock: %w", err)
	}

	done := make(chan error, 1)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("%w: %v", errReloadLockPanic, recovered)
			}
		}()

		done <- syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
	}()

	select {
	case <-ctx.Done():
		_ = lockFile.Close()

		return nil, fmt.Errorf("lock daemon reload: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			_ = lockFile.Close()

			slog.WarnContext(ctx, "daemon.client.reload_lock.lock_failed",
				"concern", "process.daemon.lifecycle", "component", "daemon",
				"lock_path", lockPath, "err", err)

			return nil, fmt.Errorf("lock reload: %w", err)
		}
	}

	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}
