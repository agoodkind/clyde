package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/slogger"
)

type supervisorLogFile struct {
	path       string
	descriptor int
	appendMode bool
	size       int64
	links      uint64
	identity   supervisorLogIdentity
}

type supervisorLogIdentity struct {
	device uint64
	inode  uint64
}

func supervisorRetentionPaths(ctx context.Context, policy slogger.CleanupPolicy) (_ slogger.CleanupPolicy, resultError error) {
	defer func() {
		if resultError != nil {
			slog.WarnContext(ctx, "daemon.retention.supervisor_logs_failed", "concern", "daemon.workers", "err", resultError)
		}
	}()
	socket := os.Getenv(daemonsupervisor.EnvSupervisorSocket)
	if socket == "" {
		return policy, nil
	}
	status, err := daemonsupervisor.RequestStatus(ctx, socket)
	if err != nil {
		return policy, fmt.Errorf("confirm supervisor for log retention: %w", err)
	}
	if status.PID <= 0 {
		return policy, fmt.Errorf("supervisor returned invalid process ID %d for log retention", status.PID)
	}
	files, err := supervisorOpenLogFiles(ctx, status.PID)
	if err != nil {
		return policy, fmt.Errorf("inspect supervisor %d open logs; defer retention: %w", status.PID, err)
	}
	updated := policy
	updated.Paths = slices.Clone(policy.Paths)
	updated.KeepInPlacePaths = slices.Clone(policy.KeepInPlacePaths)
	updated.ProtectedPaths = slices.Clone(policy.ProtectedPaths)
	retained := make(map[supervisorLogIdentity]int64)
	for _, file := range files {
		path, err := supervisorPolicyPath(policy, file.path)
		if err != nil {
			return policy, err
		}
		owned := slogger.CleanupOwnsPath(policy, path)
		stdioLog := (file.descriptor == 1 || file.descriptor == 2) && strings.HasSuffix(path, ".log")
		if !owned && !stdioLog {
			continue
		}
		if file.links == 0 {
			accountSupervisorUnlinkedFile(&updated, retained, file)
			continue
		}
		if err := includeSupervisorNamedFile(&updated, file, path, stdioLog && !owned); err != nil {
			return policy, err
		}
	}
	slog.DebugContext(ctx, "daemon.retention.supervisor_logs_inspected", "concern", "daemon.workers", "pid", status.PID, "append_logs", len(updated.KeepInPlacePaths), "protected_logs", len(updated.ProtectedPaths), "retained_open_bytes", updated.RetainedOpenBytes, "retained_open_files", updated.RetainedOpenFiles)
	return updated, nil
}

func accountSupervisorUnlinkedFile(policy *slogger.CleanupPolicy, retained map[supervisorLogIdentity]int64, file supervisorLogFile) {
	previous, known := retained[file.identity]
	if !known {
		policy.RetainedOpenFiles++
	}
	if file.size > previous {
		policy.RetainedOpenBytes += file.size - previous
		retained[file.identity] = file.size
	} else if !known {
		retained[file.identity] = file.size
	}
}

func includeSupervisorNamedFile(policy *slogger.CleanupPolicy, file supervisorLogFile, path string, external bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		slog.Warn("daemon.retention.supervisor_file_failed", "concern", "daemon.workers", "path", path, "err", err)
		return fmt.Errorf("stat supervisor log %s; defer retention: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if external && !slices.Contains(policy.Paths, path) {
		policy.Paths = append(policy.Paths, path)
	}
	if !file.appendMode {
		if !slices.Contains(policy.ProtectedPaths, path) {
			policy.ProtectedPaths = append(policy.ProtectedPaths, path)
		}
		return nil
	}
	if !slices.Contains(policy.KeepInPlacePaths, path) {
		policy.KeepInPlacePaths = append(policy.KeepInPlacePaths, path)
	}
	return nil
}

// macOS lsof resolves aliases such as /tmp to /private/tmp. Cleanup paths must
// use the policy spelling to match KeepInPlacePaths during archive selection.
func supervisorPolicyPath(policy slogger.CleanupPolicy, path string) (string, error) {
	path = filepath.Clean(path)
	roots := append([]string(nil), policy.Roots...)
	if policy.Root != "" {
		roots = append(roots, policy.Root)
	}
	for _, target := range policy.Paths {
		roots = append(roots, filepath.Dir(target))
	}
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			slog.Warn("daemon.retention.supervisor_root_failed", "concern", "daemon.workers", "root", root, "err", err)
			return "", fmt.Errorf("resolve supervisor log root %s: %w", root, err)
		}
		relative, err := filepath.Rel(resolved, path)
		if err == nil && filepath.IsLocal(relative) {
			candidate := filepath.Join(root, relative)
			if slogger.CleanupOwnsPath(policy, candidate) {
				return candidate, nil
			}
		}
	}
	return path, nil
}
