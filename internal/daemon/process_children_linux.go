//go:build linux

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func childProcessIDs(ctx context.Context, parentPID int) ([]int, error) {
	if parentPID <= 0 {
		slog.DebugContext(ctx, "daemon.status.worker_children_skipped", "concern", "process.daemon.lifecycle", "component", "daemon",
			"parent_pid", parentPID,
		)
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		slog.WarnContext(ctx, "daemon.status.worker_children_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"parent_pid", parentPID,
			"err", err,
		)
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	pids := make([]int, 0)
	for _, entry := range entries {
		pid, ok := parseProcEntryPID(entry.Name())
		if !ok {
			continue
		}
		ppid, ok := procParentPID(pid)
		if ok && ppid == parentPID {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	slog.DebugContext(ctx, "daemon.status.worker_children_found", "concern", "process.daemon.lifecycle", "component", "daemon",
		"parent_pid", parentPID,
		"count", len(pids),
	)
	return pids, nil
}

func parseProcEntryPID(name string) (int, bool) {
	pid, err := strconv.Atoi(name)
	if err != nil {
		return 0, false
	}
	return pid, pid > 0
}

func procParentPID(pid int) (int, bool) {
	statPath := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	fieldsStart := strings.LastIndex(string(data), ") ")
	if fieldsStart < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[fieldsStart+2:]))
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
