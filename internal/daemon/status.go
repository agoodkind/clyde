package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
)

// StatusReport describes the current daemon supervisor and worker ownership
// state without starting or restarting any daemon process.
type StatusReport struct {
	LaunchdTarget          string
	LaunchdError           string
	SupervisorPID          int
	SupervisorSocketPath   string
	SupervisorSocketExists bool
	SupervisorResponding   bool
	SupervisorFingerprint  string
	SupervisorError        string
	DaemonSocketPath       string
	DaemonSocketExists     bool
	DaemonResponding       bool
	DaemonError            string
	WorkerPIDs             []int
	WorkerError            string
}

// InspectStatus returns the current daemon status using only read-only probes.
func InspectStatus(ctx context.Context) StatusReport {
	report := StatusReport{
		LaunchdTarget:          "",
		LaunchdError:           "",
		SupervisorPID:          0,
		SupervisorResponding:   false,
		SupervisorFingerprint:  "",
		SupervisorError:        "",
		DaemonSocketPath:       config.DaemonSocketPath(),
		DaemonSocketExists:     pathExists(config.DaemonSocketPath()),
		DaemonResponding:       false,
		DaemonError:            "",
		SupervisorSocketPath:   supervisorSocketPath(),
		SupervisorSocketExists: pathExists(supervisorSocketPath()),
		WorkerPIDs:             nil,
		WorkerError:            "",
	}

	client, err := connect(ctx)
	if err != nil {
		report.DaemonError = err.Error()
	} else {
		report.DaemonResponding = true
		_ = client.Close()
	}
	supervisorStatus, err := daemonsupervisor.RequestStatus(ctx, report.SupervisorSocketPath)
	if err != nil {
		report.SupervisorError = err.Error()
	} else {
		report.SupervisorResponding = true
		report.SupervisorFingerprint = supervisorStatus.Fingerprint
		if report.SupervisorPID == 0 {
			report.SupervisorPID = supervisorStatus.PID
		}
	}

	if runtime.GOOS != "darwin" {
		return report
	}

	uid := strconv.Itoa(os.Getuid())
	report.LaunchdTarget = "gui/" + uid + "/" + launchAgentLabel
	supervisorPID, err := findDaemonPID(ctx)
	if err != nil {
		report.LaunchdError = err.Error()
		return report
	}
	report.SupervisorPID = supervisorPID
	if supervisorPID <= 0 {
		return report
	}

	workerPIDs, err := childProcessIDs(ctx, supervisorPID)
	if err != nil {
		report.WorkerError = err.Error()
		return report
	}
	report.WorkerPIDs = workerPIDs
	return report
}

// CompiledSupervisorFingerprint returns the fingerprint stamped into this binary.
func CompiledSupervisorFingerprint() string {
	return daemonsupervisor.CompiledFingerprint()
}

// RunningSupervisorFingerprint asks the running supervisor for its fingerprint.
func RunningSupervisorFingerprint(ctx context.Context) (string, error) {
	log := daemonClientLog(ctx)
	status, err := daemonsupervisor.RequestStatus(ctx, supervisorSocketPath())
	if err != nil {
		log.WarnContext(ctx, "daemon.status.supervisor_fingerprint_failed", "err", err)
		return "", fmt.Errorf("request running supervisor fingerprint: %w", err)
	}
	fingerprint := strings.TrimSpace(status.Fingerprint)
	if fingerprint == "" {
		log.WarnContext(ctx, "daemon.status.supervisor_fingerprint_empty")
		return "", fmt.Errorf("daemon supervisor returned an empty fingerprint")
	}
	return fingerprint, nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func childProcessIDs(ctx context.Context, parentPID int) ([]int, error) {
	log := daemonClientLog(ctx)
	out, err := clientCommandRunner.Output("pgrep", "-P", strconv.Itoa(parentPID))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && strings.TrimSpace(string(out)) == "" {
			return nil, nil
		}
		log.WarnContext(ctx, "daemon.status.worker_children_failed",
			"parent_pid", parentPID,
			"err", err)
		return nil, fmt.Errorf("list worker children for supervisor pid %d: %w", parentPID, err)
	}
	lines := strings.Split(string(out), "\n")
	pids := make([]int, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pid, convErr := strconv.Atoi(line)
		if convErr != nil {
			log.WarnContext(ctx, "daemon.status.worker_pid_parse_failed",
				"parent_pid", parentPID,
				"raw_pid", line,
				"err", convErr)
			return nil, fmt.Errorf("parse worker pid %q: %w", line, convErr)
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}
