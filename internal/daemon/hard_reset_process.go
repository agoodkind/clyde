package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
)

type resetProcess struct {
	PID              int
	Started          string
	Executable       string
	Arguments        []string
	SupervisorSocket string
}

func hardResetProcesses(ctx context.Context, executable string) (_ []resetProcess, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.hard_reset.identify_processes_failed", "concern", "process.daemon.lifecycle", "err", err)
		}
	}()
	socket := daemonsupervisor.SocketPath(config.RuntimeDir())
	supervisorPID := 0
	if socketExists(socket) {
		status, err := daemonsupervisor.RequestStatus(ctx, socket)
		if err == nil {
			supervisorPID = status.PID
		} else if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("identify reset supervisor: %w", err)
		}
	}
	var candidates []int
	if supervisorPID > 0 {
		children, err := childProcessIDs(ctx, supervisorPID)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, supervisorPID)
		candidates = append(candidates, children...)
	}
	// A previous supervisor can exit while its old generation is still draining.
	// Only exact workers carrying this runtime's supervisor socket are ours.
	orphans, err := childProcessIDs(ctx, 1)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, orphans...)
	var result []resetProcess
	for _, pid := range candidates {
		process, err := readResetProcess(ctx, pid)
		if err != nil {
			if pid == supervisorPID {
				return nil, fmt.Errorf("identify supervisor %d: %w", pid, err)
			}
			continue
		}
		if !resetProcessMatches(process, executable, socket, pid == supervisorPID) {
			if pid == supervisorPID {
				return nil, fmt.Errorf("supervisor %d does not match the current Clyde executable", pid)
			}
			continue
		}
		result = append(result, process)
	}
	return result, nil
}

func resetProcessMatches(process resetProcess, executable, socket string, supervisor bool) bool {
	expected, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(process.Executable)
	if err != nil || resolved != expected || len(process.Arguments) != 3 || process.Arguments[1] != "daemon" {
		return false
	}
	if supervisor {
		return process.Arguments[2] == "run"
	}
	return process.Arguments[2] == "worker" && process.SupervisorSocket == socket
}

func readResetProcess(ctx context.Context, pid int) (resetProcess, error) {
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "daemon.hard_reset.process_read_canceled", "concern", "process.daemon.lifecycle", "err", err)
		return resetProcess{}, fmt.Errorf("read reset process %d: %w", pid, err)
	}
	return resetProcessDetails(pid)
}

func stopResetProcesses(ctx context.Context, processes []resetProcess) (err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.hard_reset.stop_processes_failed", "concern", "process.daemon.lifecycle", "err", err)
		}
	}()
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		for _, process := range processes {
			alive, err := sameResetProcess(ctx, process)
			if err != nil {
				return err
			}
			if !alive {
				continue
			}
			if err := syscall.Kill(process.PID, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("stop Clyde process %d: %w", process.PID, err)
			}
		}
		deadline := time.NewTimer(daemonShutdownTimeout)
		ticker := time.NewTicker(50 * time.Millisecond)
		stopped, err := awaitResetProcesses(ctx, processes, ticker.C, deadline.C)
		ticker.Stop()
		deadline.Stop()
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}
	return fmt.Errorf("clyde processes did not exit; reset data was preserved")
}

func awaitResetProcesses(ctx context.Context, processes []resetProcess, tick <-chan time.Time, deadline <-chan time.Time) (_ bool, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.hard_reset.await_processes_failed", "concern", "process.daemon.lifecycle", "err", err)
		}
	}()
	for {
		remaining := false
		for _, process := range processes {
			alive, err := sameResetProcess(ctx, process)
			if err != nil {
				return false, err
			}
			remaining = remaining || alive
		}
		if !remaining {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("await reset processes: %w", ctx.Err())
		case <-deadline:
			return false, nil
		case <-tick:
		}
	}
}

func sameResetProcess(ctx context.Context, expected resetProcess) (_ bool, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.hard_reset.verify_process_failed", "concern", "process.daemon.lifecycle", "pid", expected.PID, "err", err)
		}
	}()
	if err := syscall.Kill(expected.PID, 0); errors.Is(err, syscall.ESRCH) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("probe reset process %d: %w", expected.PID, err)
	}
	current, err := resetProcessOwnedStart(expected.PID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, fmt.Errorf("verify Clyde process %d before deletion: %w", expected.PID, err)
	}
	return current == expected.Started, nil
}
