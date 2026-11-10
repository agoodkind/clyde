//go:build !darwin && !linux

package daemon

import (
	"context"
	"log/slog"
)

func childProcessIDs(ctx context.Context, parentPID int) ([]int, error) {
	slog.DebugContext(ctx, "daemon.status.worker_children_unsupported", "concern", "process.daemon.lifecycle", "component", "daemon",
		"parent_pid", parentPID,
	)
	return nil, nil
}
