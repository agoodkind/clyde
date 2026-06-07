package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
)

const reloadCommandTimeout = 75 * time.Second

var (
	runCommand          = daemonsvc.RunCommand
	runWorker           = daemonsvc.Run
	reloadDaemon        = daemonsvc.ReloadDaemon
	inspectDaemonStatus = daemonsvc.InspectStatus
	builtFingerprint    = daemonsvc.CompiledSupervisorFingerprint
	runningFingerprint  = daemonsvc.RunningSupervisorFingerprint
)

// NewCmd is part of Clyde's typed adapter surface.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "daemon",
		Short:   "Manage the background daemon",
		Long:    "Manage the background daemon process and its control-plane commands.",
		Example: "clyde daemon run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newRunCmd(f))
	cmd.AddCommand(newWorkerCmd(f))
	cmd.AddCommand(newDeployCmd(f))
	cmd.AddCommand(newReloadCmd(f))
	cmd.AddCommand(newStatusCmd(f))
	cmd.AddCommand(newFingerprintCmd(f))
	return cmd
}

func newRunCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "run",
		Short:   "Start the background daemon",
		Long:    "Start the background daemon process. launchd owns the daemon's lifecycle, so this entry point is normally invoked by the launch agent rather than directly.",
		Example: "clyde daemon run",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Info("cli.daemon.invoked", "concern", "cli.daemon", "component", "cli",
				"version", f.Build.Version,
			)
			log := slog.Default().With("component", "daemon")
			return runCommand(log)
		},
	}
}

func newWorkerCmd(_ *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "worker",
		Short:   "Run the daemon worker service",
		Long:    "Run the daemon worker service, the process that owns the public listeners. The supervisor spawns the worker during startup and reload; it is not normally run directly.",
		Example: "clyde daemon worker",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Info("cli.daemon.worker.invoked", "concern", "cli.daemon", "component", "cli")
			log := slog.Default().With("component", "daemon")
			return runWorker(log)
		},
	}
}

func newReloadCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "reload",
		Short:   "Reload the running daemon without restarting it",
		Long:    "Reload the running daemon in place, handing its live listeners to a supervisor-spawned replacement worker so public traffic keeps flowing across the swap.",
		Example: "clyde daemon reload",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), reloadCommandTimeout)
			defer cancel()
			resp, err := reloadDaemon(ctx)
			if err != nil {
				return reloadCommandError(err)
			}
			status := "unchanged"
			if resp.GetBinaryReloaded() {
				status = "reloaded"
			}
			body := fmt.Sprintf("daemon binary %s: active_surfaces=%d new_pid=%d\n", status, resp.GetActiveSurfaces(), resp.GetNewPid())
			return response.WriteText(cmd.Context(), f.IOStreams.Out, body)
		},
	}
}

func reloadCommandError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("daemon reload did not complete within " + reloadCommandTimeout.String() + " and no reload confirmation was received; check `clyde daemon status` and the daemon log, then rerun `clyde daemon reload`: " + err.Error())
	}
	return err
}

func newStatusCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Report daemon supervisor and worker status",
		Long:    "Report whether the launch agent, the supervisor, and the worker are reachable, including the control socket paths the CLI uses to reach them.",
		Example: "clyde daemon status",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			report := inspectDaemonStatus(ctx)
			var body strings.Builder
			writeStatusReport(&body, report)
			return response.WriteText(cmd.Context(), f.IOStreams.Out, body.String())
		},
	}
}

func newFingerprintCmd(f *cli.Factory) *cobra.Command {
	var built bool
	cmd := &cobra.Command{
		Use:     "fingerprint",
		Short:   "Print the supervisor fingerprint",
		Long:    "Print the supervisor fingerprint, the build identity the supervisor compares across a reload to decide whether the daemon binary changed.",
		Example: "clyde daemon fingerprint",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if built {
				return response.WriteText(cmd.Context(), f.IOStreams.Out, builtFingerprint()+"\n")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			fingerprint, err := runningFingerprint(ctx)
			if err != nil {
				return err
			}
			return response.WriteText(cmd.Context(), f.IOStreams.Out, fingerprint+"\n")
		},
	}
	cmd.Flags().BoolVar(&built, "built", false, "print the compiled supervisor fingerprint")
	return cmd
}

func writeStatusReport(out io.Writer, report daemonsvc.StatusReport) {
	_, _ = fmt.Fprintln(out, "daemon status")
	switch {
	case report.LaunchdTarget == "":
		_, _ = fmt.Fprintln(out, "launchd: unavailable")
	case report.LaunchdError != "":
		_, _ = fmt.Fprintf(out, "launchd: unavailable target=%s error=%s\n", report.LaunchdTarget, report.LaunchdError)
	case report.SupervisorPID > 0:
		_, _ = fmt.Fprintf(out, "launchd: running target=%s supervisor_pid=%d\n", report.LaunchdTarget, report.SupervisorPID)
	default:
		_, _ = fmt.Fprintf(out, "launchd: loaded target=%s supervisor_pid=none\n", report.LaunchdTarget)
	}

	daemonSocketState := "missing"
	if report.DaemonSocketExists {
		daemonSocketState = "present"
	}
	_, _ = fmt.Fprintf(out, "daemon_socket: %s path=%s\n", daemonSocketState, report.DaemonSocketPath)

	switch {
	case report.DaemonResponding:
		_, _ = fmt.Fprintf(out, "daemon_rpc: responding socket=%s\n", report.DaemonSocketPath)
	case report.DaemonError != "":
		_, _ = fmt.Fprintf(out, "daemon_rpc: unavailable socket=%s error=%s\n", report.DaemonSocketPath, report.DaemonError)
	default:
		_, _ = fmt.Fprintf(out, "daemon_rpc: unavailable socket=%s\n", report.DaemonSocketPath)
	}

	supervisorSocketState := "missing"
	if report.SupervisorSocketExists {
		supervisorSocketState = "present"
	}
	_, _ = fmt.Fprintf(out, "supervisor_socket: %s path=%s\n", supervisorSocketState, report.SupervisorSocketPath)
	switch {
	case report.SupervisorResponding && report.SupervisorFingerprint != "":
		_, _ = fmt.Fprintf(out, "supervisor_rpc: responding socket=%s fingerprint=%s\n", report.SupervisorSocketPath, report.SupervisorFingerprint)
	case report.SupervisorResponding:
		_, _ = fmt.Fprintf(out, "supervisor_rpc: responding socket=%s fingerprint=unknown\n", report.SupervisorSocketPath)
	case report.SupervisorError != "":
		_, _ = fmt.Fprintf(out, "supervisor_rpc: unavailable socket=%s error=%s\n", report.SupervisorSocketPath, report.SupervisorError)
	default:
		_, _ = fmt.Fprintf(out, "supervisor_rpc: unavailable socket=%s\n", report.SupervisorSocketPath)
	}

	if len(report.WorkerPIDs) > 0 {
		_, _ = fmt.Fprintf(out, "worker: pids=%s\n", joinPIDs(report.WorkerPIDs))
		return
	}
	if report.WorkerError != "" {
		_, _ = fmt.Fprintf(out, "worker: unknown error=%s\n", report.WorkerError)
		return
	}
	_, _ = fmt.Fprintln(out, "worker: none")
}

func joinPIDs(pids []int) string {
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, strconv.Itoa(pid))
	}
	return strings.Join(parts, ",")
}
