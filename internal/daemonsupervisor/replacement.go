package daemonsupervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"syscall"
)

func admitReplacement(ctx context.Context, log *slog.Logger, handle workerHandle, ready io.Reader, forward *os.File, replacementCh chan<- workerHandle) {
	if err := waitForWorkerReady(ctx, ready, handle.waitCh); err != nil {
		log.WarnContext(ctx, "daemon.supervisor.reload_replacement_rejected", "concern", "process.daemon.lifecycle", "pid", handle.cmd.Process.Pid, "err", err)
		if !errors.Is(err, errWorkerExitedBeforeReady) {
			stopWorker(log, handle.cmd, syscall.SIGTERM, handle.waitCh)
		}
		return
	}
	select {
	case replacementCh <- handle:
	case <-ctx.Done():
		stopWorker(log, handle.cmd, syscall.SIGTERM, handle.waitCh)
		return
	}
	if _, err := forward.WriteString("ready\n"); err != nil {
		log.WarnContext(ctx, "daemon.supervisor.reload_ready_forward_failed", "concern", "process.daemon.lifecycle", "pid", handle.cmd.Process.Pid, "err", err)
	}
}
