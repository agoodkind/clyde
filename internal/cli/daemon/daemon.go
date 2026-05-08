package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

var (
	runCommand          = daemonsvc.RunCommand
	runWorker           = daemonsvc.Run
	reloadDaemon        = daemonsvc.ReloadDaemon
	inspectDaemonStatus = daemonsvc.InspectStatus
)

func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "daemon",
		Short:  "Start the background daemon (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Info("cli.daemon.invoked",
				"component", "cli",
				"version", f.Build.Version,
			)
			log := slog.Default().With("component", "daemon")
			return runCommand(log, pruneLoop(), oauthLoop(), driftLoop())
		},
	}
	cmd.AddCommand(newWorkerCmd(f))
	cmd.AddCommand(newReloadCmd(f))
	cmd.AddCommand(newStatusCmd(f))
	cmd.AddCommand(newLaunchRemoteWorkerCmd(f))
	return cmd
}

func newWorkerCmd(_ *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:    "worker",
		Short:  "Run the daemon worker service",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Info("cli.daemon.worker.invoked",
				"component", "cli",
			)
			log := slog.Default().With("component", "daemon")
			return runWorker(log, pruneLoop(), oauthLoop(), driftLoop())
		},
	}
}

func newReloadCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "reload",
		Short: "Reload the running daemon without restarting it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 12*time.Second)
			defer cancel()
			resp, err := reloadDaemon(ctx)
			if err != nil {
				return err
			}
			status := "unchanged"
			if resp.GetBinaryReloaded() {
				status = "reloaded"
			}
			_, _ = fmt.Fprintf(f.IOStreams.Out, "daemon binary %s: active_sessions=%d new_pid=%d\n", status, resp.GetActiveSessions(), resp.GetNewPid())
			return nil
		},
	}
}

func newStatusCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report daemon supervisor and worker status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			report := inspectDaemonStatus(ctx)
			writeStatusReport(f, report)
			return nil
		},
	}
}

func writeStatusReport(f *cli.Factory, report daemonsvc.StatusReport) {
	out := f.IOStreams.Out
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
