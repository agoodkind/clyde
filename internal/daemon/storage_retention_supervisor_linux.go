//go:build linux

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func supervisorOpenLogFiles(ctx context.Context, pid int) (_ []supervisorLogFile, resultError error) {
	defer func() {
		if resultError != nil {
			slog.WarnContext(ctx, "daemon.retention.supervisor_proc_failed", "concern", "daemon.workers", "pid", pid, "err", resultError)
		}
	}()
	slog.DebugContext(ctx, "daemon.retention.supervisor_proc_started", "concern", "daemon.workers", "pid", pid)
	root := filepath.Join("/proc", strconv.Itoa(pid))
	entries, err := os.ReadDir(filepath.Join(root, "fd"))
	if err != nil {
		return nil, fmt.Errorf("list supervisor descriptors: %w", err)
	}
	var files []supervisorLogFile
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("inspect supervisor log descriptors: %w", err)
		}
		descriptor, err := strconv.Atoi(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("parse supervisor descriptor %s: %w", entry.Name(), err)
		}
		link := filepath.Join(root, "fd", entry.Name())
		info, err := os.Stat(link)
		if err != nil {
			return nil, fmt.Errorf("stat supervisor descriptor %d: %w", descriptor, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("supervisor descriptor %d does not report inode metadata", descriptor)
		}
		path, err := os.Readlink(link)
		if err != nil {
			return nil, fmt.Errorf("read supervisor descriptor %d: %w", descriptor, err)
		}
		if stat.Nlink == 0 {
			path = strings.TrimSuffix(path, " (deleted)")
		}
		data, err := os.ReadFile(filepath.Join(root, "fdinfo", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read supervisor descriptor %d flags: %w", descriptor, err)
		}
		flags, err := readSupervisorDescriptorFlags(data, descriptor)
		if err != nil {
			return nil, err
		}
		if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
			files = append(files, supervisorLogFile{path: path, descriptor: descriptor, appendMode: flags&syscall.O_APPEND != 0, size: info.Size(), links: uint64(stat.Nlink), identity: supervisorLogIdentity{device: stat.Dev, inode: stat.Ino}})
		}
	}
	return files, nil
}

func readSupervisorDescriptorFlags(data []byte, descriptor int) (uint64, error) {
	for line := range strings.SplitSeq(string(data), "\n") {
		value, ok := strings.CutPrefix(line, "flags:")
		if !ok {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimSpace(value), 8, 64)
		if err != nil {
			slog.Warn("daemon.retention.supervisor_flags_failed", "concern", "daemon.workers", "descriptor", descriptor, "err", err)
			return 0, fmt.Errorf("parse supervisor descriptor %d flags: %w", descriptor, err)
		}
		return flags, nil
	}
	return 0, fmt.Errorf("supervisor descriptor %d does not report file flags", descriptor)
}
