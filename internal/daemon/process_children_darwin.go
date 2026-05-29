//go:build darwin

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"golang.org/x/sys/unix"
)

const maxDarwinProcessParentID = int64(1<<31 - 1)

func childProcessIDs(ctx context.Context, parentPID int) ([]int, error) {
	if parentPID <= 0 {
		slog.DebugContext(ctx, "daemon.status.worker_children_skipped", "concern", "process.daemon.lifecycle", "component", "daemon",
			"parent_pid", parentPID,
		)
		return nil, nil
	}
	if int64(parentPID) > maxDarwinProcessParentID {
		err := fmt.Errorf("parent pid %d exceeds Darwin process id range", parentPID)
		slog.WarnContext(ctx, "daemon.status.worker_children_invalid_parent", "concern", "process.daemon.lifecycle", "component", "daemon",
			"parent_pid", parentPID,
			"err", err,
		)
		return nil, err
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		slog.WarnContext(ctx, "daemon.status.worker_children_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"parent_pid", parentPID,
			"err", err,
		)
		return nil, fmt.Errorf("list Darwin process table: %w", err)
	}
	pids := make([]int, 0)
	for _, process := range processes {
		if process.Eproc.Ppid == int32(parentPID) {
			pids = append(pids, int(process.Proc.P_pid))
		}
	}
	sort.Ints(pids)
	slog.DebugContext(ctx, "daemon.status.worker_children_found", "concern", "process.daemon.lifecycle", "component", "daemon",
		"parent_pid", parentPID,
		"count", len(pids),
	)
	return pids, nil
}
