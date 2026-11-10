package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/clyde/internal/deploy"
)

// Uninstall stops Clyde daemon processes and removes only the native service
// registration. It preserves all Clyde data, configuration, and binaries.
func Uninstall(ctx context.Context, output io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		slog.WarnContext(ctx, "daemon.uninstall.executable_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return fmt.Errorf("resolve Clyde executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		slog.WarnContext(ctx, "daemon.uninstall.symlink_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return fmt.Errorf("resolve Clyde executable symlinks: %w", err)
	}
	lookup := func(key string) (string, bool) {
		if key == "INSTALL_BIN" {
			return executable, true
		}
		return os.LookupEnv(key)
	}
	processes, err := hardResetProcesses(ctx, executable)
	if err != nil {
		slog.WarnContext(ctx, "daemon.uninstall.process_lookup_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return err
	}
	if err := deploy.RemoveFromEnv(ctx, lookup, output); err != nil {
		slog.WarnContext(ctx, "daemon.uninstall.registration_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return fmt.Errorf("unregister Clyde daemon: %w", err)
	}
	if err := stopResetProcesses(ctx, processes); err != nil {
		slog.WarnContext(ctx, "daemon.uninstall.stop_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return err
	}
	remaining, err := hardResetProcesses(ctx, executable)
	if err != nil {
		slog.WarnContext(ctx, "daemon.uninstall.remaining_process_lookup_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return err
	}
	if err := stopResetProcesses(ctx, remaining); err != nil {
		slog.WarnContext(ctx, "daemon.uninstall.remaining_stop_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "err", err)
		return err
	}
	_, _ = fmt.Fprintln(output, "Clyde daemon uninstalled.")
	return nil
}
