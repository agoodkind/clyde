//go:build linux

package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"goodkind.io/clyde/internal/daemonsupervisor"
)

func resetProcessDetails(pid int) (_ resetProcess, err error) {
	defer func() {
		if err != nil && !os.IsNotExist(err) {
			slog.Warn("daemon.hard_reset.process_details_failed", "concern", "process.daemon.lifecycle", "pid", pid, "err", err)
		}
	}()
	root := filepath.Join("/proc", strconv.Itoa(pid))
	info, err := os.Stat(root)
	if err != nil {
		return resetProcess{}, fmt.Errorf("stat reset process %d: %w", pid, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Getuid()) {
		return resetProcess{}, fmt.Errorf("process %d belongs to another user", pid)
	}
	started, err := resetProcessStart(pid)
	if err != nil {
		return resetProcess{}, err
	}
	info, err = os.Stat(root)
	if err != nil {
		return resetProcess{}, fmt.Errorf("recheck reset process %d ownership: %w", pid, err)
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Getuid()) {
		return resetProcess{}, fmt.Errorf("process %d belongs to another user", pid)
	}
	currentStarted, err := resetProcessStart(pid)
	if err != nil {
		return resetProcess{}, err
	}
	if currentStarted != started {
		return resetProcess{}, os.ErrNotExist
	}
	executable, err := os.Readlink(filepath.Join(root, "exe"))
	if err != nil {
		return resetProcess{}, fmt.Errorf("read reset process executable %d: %w", pid, err)
	}
	arguments, err := os.ReadFile(filepath.Join(root, "cmdline"))
	if err != nil {
		return resetProcess{}, fmt.Errorf("read reset process arguments %d: %w", pid, err)
	}
	environment, err := os.ReadFile(filepath.Join(root, "environ"))
	if err != nil {
		return resetProcess{}, fmt.Errorf("read reset process environment %d: %w", pid, err)
	}
	result := resetProcess{PID: pid, Started: started, Executable: strings.TrimSuffix(executable, " (deleted)"), Arguments: strings.Split(strings.TrimSuffix(string(arguments), "\x00"), "\x00"), SupervisorSocket: ""}
	for entry := range strings.SplitSeq(string(environment), "\x00") {
		if value, ok := strings.CutPrefix(entry, daemonsupervisor.EnvSupervisorSocket+"="); ok {
			result.SupervisorSocket = value
		}
	}
	return result, nil
}

func resetProcessStart(pid int) (string, error) {
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("daemon.hard_reset.process_stat_failed", "concern", "process.daemon.lifecycle", "pid", pid, "err", err)
		}
		return "", fmt.Errorf("read reset process start %d: %w", pid, err)
	}
	end := strings.LastIndex(string(status), ") ")
	if end < 0 {
		return "", fmt.Errorf("process %d stat is malformed", pid)
	}
	fields := strings.Fields(string(status[end+2:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("process %d start time is unavailable", pid)
	}
	if fields[0] == "Z" {
		return "", os.ErrNotExist
	}
	return fields[19], nil
}

func resetProcessOwnedStart(pid int) (string, error) {
	root := filepath.Join("/proc", strconv.Itoa(pid))
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Getuid()) {
		return "", fmt.Errorf("process %d belongs to another user", pid)
	}
	started, err := resetProcessStart(pid)
	if err != nil {
		return "", err
	}
	info, err = os.Stat(root)
	if err != nil {
		slog.Warn("daemon.hard_reset.process_ownership_recheck_failed", "concern", "process.daemon.lifecycle", "pid", pid, "err", err)
		return "", fmt.Errorf("recheck reset process %d ownership: %w", pid, err)
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Getuid()) {
		return "", fmt.Errorf("process %d belongs to another user", pid)
	}
	currentStarted, err := resetProcessStart(pid)
	if err != nil {
		return "", err
	}
	if currentStarted != started {
		return "", os.ErrNotExist
	}
	return started, nil
}
