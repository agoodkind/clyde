package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemonsupervisor"
)

// StatusReport is the CLI-facing daemon status snapshot.
type StatusReport struct {
	LaunchdTarget          string
	LaunchdError           string
	SupervisorPID          int
	DaemonSocketPath       string
	DaemonSocketExists     bool
	DaemonResponding       bool
	DaemonError            string
	SupervisorSocketPath   string
	SupervisorSocketExists bool
	SupervisorResponding   bool
	SupervisorError        string
	SupervisorFingerprint  string
	WorkerPIDs             []int
	WorkerError            string
}

// InspectStatus reports the current supervisor and worker status.
func InspectStatus(ctx context.Context) StatusReport {
	runtimeDir := config.RuntimeDir()
	supervisorSocketPath := daemonsupervisor.SocketPath(runtimeDir)
	daemonAddress := daemonGRPCAddress()
	daemonSocketPath, err := config.DaemonSocketPathFromGRPCAddress(daemonAddress)
	if err != nil {
		daemonSocketPath = config.DaemonSocketPath()
	}
	report := StatusReport{
		LaunchdTarget:          "",
		LaunchdError:           "",
		SupervisorPID:          0,
		DaemonSocketPath:       daemonSocketPath,
		DaemonSocketExists:     socketExists(daemonSocketPath),
		DaemonResponding:       false,
		DaemonError:            "",
		SupervisorSocketPath:   supervisorSocketPath,
		SupervisorSocketExists: socketExists(supervisorSocketPath),
		SupervisorResponding:   false,
		SupervisorError:        "",
		SupervisorFingerprint:  "",
		WorkerPIDs:             nil,
		WorkerError:            "",
	}
	status, err := daemonsupervisor.RequestStatus(ctx, supervisorSocketPath)
	if err != nil {
		report.SupervisorError = err.Error()
	} else {
		report.SupervisorResponding = true
		report.SupervisorPID = status.PID
		report.SupervisorFingerprint = status.Fingerprint
	}
	if err := probeDaemonRPC(ctx); err != nil {
		report.DaemonError = err.Error()
	} else {
		report.DaemonResponding = true
	}
	if runtime.GOOS == "darwin" {
		report.LaunchdTarget = "gui/" + strconv.Itoa(os.Getuid()) + "/io.goodkind.clyde.daemon"
	}
	if report.SupervisorPID > 0 {
		workerPIDs, err := childProcessIDs(ctx, report.SupervisorPID)
		if err != nil {
			report.WorkerError = err.Error()
		} else {
			report.WorkerPIDs = workerPIDs
		}
	}
	return report
}

// CompiledSupervisorFingerprint returns the build-stamped supervisor fingerprint.
func CompiledSupervisorFingerprint() string {
	return daemonsupervisor.CompiledFingerprint()
}

// RunningSupervisorFingerprint returns the running supervisor fingerprint.
func RunningSupervisorFingerprint(ctx context.Context) (string, error) {
	socketPath := daemonsupervisor.SocketPath(config.RuntimeDir())
	if !socketExists(socketPath) {
		return "", errNoSupervisorSocket(socketPath)
	}
	status, err := daemonsupervisor.RequestStatus(ctx, socketPath)
	if err != nil {
		slog.WarnContext(ctx, "daemon.supervisor.status_failed", "concern", "process.daemon.lifecycle", "component", "daemon", "socket_path", socketPath, "err", err)
		return "", fmt.Errorf("request supervisor status: %w", err)
	}
	return status.Fingerprint, nil
}
