package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

// ReloadCommandTimeout is the shared timeout for daemon reload command paths.
const ReloadCommandTimeout = 75 * time.Second

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
	cmd.AddCommand(newBackfillConversationScalarsCmd(f))
	return cmd
}

// ReloadCommandError normalizes reload command errors into user-facing wording.
func ReloadCommandError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("daemon reload did not complete within " + ReloadCommandTimeout.String() + " and no reload confirmation was received; check `clyde daemon status` and the daemon log, then rerun `clyde daemon reload`: " + err.Error())
	}
	return err
}

// WriteStatusReport writes the human-readable daemon status report.
func WriteStatusReport(out io.Writer, report daemonsvc.StatusReport) {
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
