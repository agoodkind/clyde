package daemon

import (
	"fmt"
	"log/slog"
	"runtime"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/slogger"
)

// RunCommand owns the platform-specific daemon command entrypoint.
// On Darwin, the command process is the launchd-owned supervisor; on other
// platforms it runs the daemon service directly.
func RunCommand(log *slog.Logger, extraLoops ...ExtraLoop) error {
	if runtime.GOOS != "darwin" {
		return Run(log, extraLoops...)
	}
	log = slogger.WithConcern(log, slogger.ConcernProcessDaemonLifecycle)
	if err := config.EnsureRuntimeDir(); err != nil {
		log.Warn("daemon.supervisor.runtime_dir_failed", "component", "daemon", "err", err)
		return fmt.Errorf("ensure daemon runtime dir: %w", err)
	}
	if err := daemonsupervisor.Supervise(log, config.RuntimeDir()); err != nil {
		return fmt.Errorf("supervise daemon under launchd: %w", err)
	}
	return nil
}

func supervisorSocketPath() string {
	return daemonsupervisor.SocketPath(config.RuntimeDir())
}
