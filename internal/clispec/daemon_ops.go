package clispec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	daemoncmd "goodkind.io/clyde/internal/cli/daemon"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

type daemonStatusInput struct {
	TimeoutSeconds int
}

func (daemonStatusInput) isClispecInput() {}

type daemonStatusPayload struct {
	Timeout time.Duration
}

func (daemonStatusPayload) isClispecPrepared() {}

type daemonFingerprintInput struct {
	Built bool
}

func (daemonFingerprintInput) isClispecInput() {}

type daemonFingerprintPayload struct {
	Built bool
}

func (daemonFingerprintPayload) isClispecPrepared() {}

type daemonStatusOutput struct {
	LaunchdTarget          string `json:"launchd_target"`
	LaunchdError           string `json:"launchd_error,omitempty"`
	SupervisorPID          int    `json:"supervisor_pid"`
	DaemonSocketPath       string `json:"daemon_socket_path"`
	DaemonSocketExists     bool   `json:"daemon_socket_exists"`
	DaemonResponding       bool   `json:"daemon_responding"`
	DaemonError            string `json:"daemon_error,omitempty"`
	SupervisorSocketPath   string `json:"supervisor_socket_path"`
	SupervisorSocketExists bool   `json:"supervisor_socket_exists"`
	SupervisorResponding   bool   `json:"supervisor_responding"`
	SupervisorError        string `json:"supervisor_error,omitempty"`
	SupervisorFingerprint  string `json:"supervisor_fingerprint,omitempty"`
	WorkerPIDs             []int  `json:"worker_pids,omitempty"`
	WorkerError            string `json:"worker_error,omitempty"`
}

func (daemonStatusOutput) isClispecStructuredPayload() {}

type daemonFingerprintOutput struct {
	Built       bool   `json:"built"`
	Fingerprint string `json:"fingerprint"`
}

func (daemonFingerprintOutput) isClispecStructuredPayload() {}

type daemonReloadInput struct {
	TimeoutSeconds int
}

func (daemonReloadInput) isClispecInput() {}

type daemonReloadPayload struct {
	Timeout time.Duration
}

func (daemonReloadPayload) isClispecPrepared() {}

type daemonReloadOutput struct {
	Status         string `json:"status"`
	BinaryReloaded bool   `json:"binary_reloaded"`
	ActiveSurfaces int64  `json:"active_surfaces"`
	NewPID         int64  `json:"new_pid"`
}

func (daemonReloadOutput) isClispecStructuredPayload() {}

var daemonGroup = &Group{
	Use:     "daemon",
	Short:   "Manage the background daemon",
	Long:    "Manage the background daemon process and its control-plane commands.",
	Example: "clyde daemon run",
	Parent:  nil,
}

func daemonStatusOp() Operation[daemonStatusInput, daemonStatusPayload] {
	return Operation[daemonStatusInput, daemonStatusPayload]{
		Name:       Name{Canonical: "daemon_status", CLIOverride: "status"},
		Group:      daemonGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Report daemon supervisor and worker status",
		Long:       "Report whether the launch agent, the supervisor, and the worker are reachable, including the control socket paths the CLI uses to reach them.",
		Examples:   []string{"clyde daemon status"},
		Args:       nil,
		Params:     nil,
		New: func() daemonStatusInput {
			return daemonStatusInput{TimeoutSeconds: 3}
		},
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(in daemonStatusInput) (daemonStatusPayload, error) {
			return daemonStatusPayload{Timeout: time.Duration(in.TimeoutSeconds) * time.Second}, nil
		},
		Run: nil,
		runResult: func(ctx context.Context, payload daemonStatusPayload) (Result, error) {
			statusCtx, cancel := context.WithTimeout(ctx, payload.Timeout)
			defer cancel()
			report := daemonsvc.InspectStatus(statusCtx)
			var body bytes.Buffer
			daemoncmd.WriteStatusReport(&body, report)
			return valueResult{
				Payload: daemonStatusOutput{
					LaunchdTarget:          report.LaunchdTarget,
					LaunchdError:           report.LaunchdError,
					SupervisorPID:          report.SupervisorPID,
					DaemonSocketPath:       report.DaemonSocketPath,
					DaemonSocketExists:     report.DaemonSocketExists,
					DaemonResponding:       report.DaemonResponding,
					DaemonError:            report.DaemonError,
					SupervisorSocketPath:   report.SupervisorSocketPath,
					SupervisorSocketExists: report.SupervisorSocketExists,
					SupervisorResponding:   report.SupervisorResponding,
					SupervisorError:        report.SupervisorError,
					SupervisorFingerprint:  report.SupervisorFingerprint,
					WorkerPIDs:             report.WorkerPIDs,
					WorkerError:            report.WorkerError,
				},
				Text: body.String(),
			}, nil
		},
	}
}

func daemonFingerprintOp() Operation[daemonFingerprintInput, daemonFingerprintPayload] {
	return Operation[daemonFingerprintInput, daemonFingerprintPayload]{
		Name:       Name{Canonical: "daemon_fingerprint", CLIOverride: "fingerprint"},
		Group:      daemonGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Print the supervisor fingerprint",
		Long:       "Print the supervisor fingerprint, the build identity the supervisor compares across a reload to decide whether the daemon binary changed.",
		Examples:   []string{"clyde daemon fingerprint"},
		Args:       nil,
		Params: []Param[daemonFingerprintInput]{
			BoolParam("built", "Print the compiled supervisor fingerprint.", false,
				func(in *daemonFingerprintInput, v bool) { in.Built = v }),
		},
		New:            func() daemonFingerprintInput { return daemonFingerprintInput{Built: false} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(in daemonFingerprintInput) (daemonFingerprintPayload, error) {
			return daemonFingerprintPayload(in), nil
		},
		Run: nil,
		runResult: func(ctx context.Context, p daemonFingerprintPayload) (Result, error) {
			if p.Built {
				fingerprint := daemonsvc.CompiledSupervisorFingerprint()
				return valueResult{
					Payload: daemonFingerprintOutput{Built: true, Fingerprint: fingerprint},
					Text:    fingerprint + "\n",
				}, nil
			}
			statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			fingerprint, err := daemonsvc.RunningSupervisorFingerprint(statusCtx)
			if err != nil {
				slog.WarnContext(ctx, "cli.daemon.fingerprint.failed", "concern", "cli.daemon", "component", "clispec", "err", err)
				return nil, fmt.Errorf("daemon fingerprint: %w", err)
			}
			return valueResult{
				Payload: daemonFingerprintOutput{Built: false, Fingerprint: fingerprint},
				Text:    fingerprint + "\n",
			}, nil
		},
	}
}

func daemonReloadOp() Operation[daemonReloadInput, daemonReloadPayload] {
	return Operation[daemonReloadInput, daemonReloadPayload]{
		Name:       Name{Canonical: "daemon_reload", CLIOverride: "reload"},
		Group:      daemonGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Reload the running daemon without restarting it",
		Long:       "Reload the running daemon in place, handing its live listeners to a supervisor-spawned replacement worker so public traffic keeps flowing across the swap.",
		Examples:   []string{"clyde daemon reload"},
		Args:       nil,
		Params:     nil,
		New: func() daemonReloadInput {
			return daemonReloadInput{TimeoutSeconds: int(daemoncmd.ReloadCommandTimeout / time.Second)}
		},
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(in daemonReloadInput) (daemonReloadPayload, error) {
			return daemonReloadPayload{Timeout: time.Duration(in.TimeoutSeconds) * time.Second}, nil
		},
		Run: nil,
		runResult: func(ctx context.Context, payload daemonReloadPayload) (Result, error) {
			reloadCtx, cancel := context.WithTimeout(ctx, payload.Timeout)
			defer cancel()
			resp, err := daemonsvc.ReloadDaemon(reloadCtx)
			if err != nil {
				return nil, daemoncmd.ReloadCommandError(err)
			}
			status := "unchanged"
			if resp.GetBinaryReloaded() {
				status = "reloaded"
			}
			return valueResult{
				Payload: daemonReloadOutput{
					Status:         status,
					BinaryReloaded: resp.GetBinaryReloaded(),
					ActiveSurfaces: resp.GetActiveSurfaces(),
					NewPID:         resp.GetNewPid(),
				},
				Text: fmt.Sprintf("daemon binary %s: active_surfaces=%d new_pid=%d\n", status, resp.GetActiveSurfaces(), resp.GetNewPid()),
			}, nil
		},
	}
}
