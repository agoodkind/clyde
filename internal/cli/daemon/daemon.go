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
	cmd.AddCommand(newBackfillConversationDocumentsCmd(f))
	cmd.AddCommand(newSandboxCmd(f))
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

// WriteMetricsHistoryReport appends a concise retained-log metrics report.
func WriteMetricsHistoryReport(out io.Writer, report daemonsvc.MetricsHistoryReport) {
	_, _ = fmt.Fprintf(out, "metrics_window: since=%s until=%s\n", report.Window.Since.Format(time.RFC3339), report.Window.Until.Format(time.RFC3339))
	_, _ = fmt.Fprintf(out, "metrics_coverage: complete=%t\n", report.Coverage.Complete)
	writeMetricCounter(out, "requests", report.Metrics.Requests)
	writeMetricCounter(out, "completed", report.Metrics.Completed)
	writeMetricCounter(out, "failed", report.Metrics.Failed)
	writeMetricCounter(out, "cancelled", report.Metrics.Cancelled)
	writeMetricCounter(out, "bytes_in", report.Metrics.BytesIn)
	writeMetricCounter(out, "bytes_out", report.Metrics.BytesOut)
	writeMetricCounter(out, "input_tokens", report.Metrics.InputTokens)
	writeMetricCounter(out, "output_tokens", report.Metrics.OutputTokens)
	writeMetricCounter(out, "cache_tokens", report.Metrics.CacheTokens)
	writeMetricCounter(out, "cost_microcents", report.Metrics.CostMicrocents)
	writeMetricGauge(out, "inflight", report.Metrics.Inflight)
	writeMetricGauge(out, "streaming", report.Metrics.Streaming)
	writeMetricDuration(out, "time_total", "", report.TimeBreakdown.Total)
	for _, stage := range report.TimeBreakdown.Stages {
		writeMetricDuration(out, "time_stage", stage.Name, stage.MetricsDuration)
	}
	_, _ = fmt.Fprintf(out, "unattributed_duration_ms: %s\n", formatMetricInt(report.UnattributedDurationMS))
	for _, warning := range report.Warnings {
		_, _ = fmt.Fprintf(out, "warning: %s\n", warning)
	}
}

func writeMetricCounter(out io.Writer, name string, counter daemonsvc.MetricsCounter) {
	_, _ = fmt.Fprintf(out, "%s: current=%s delta=%s rate_per_second=%s\n", name,
		formatMetricInt(counter.Current), formatMetricInt(counter.Delta), formatMetricFloat(counter.RatePerSecond))
}

func writeMetricGauge(out io.Writer, name string, gauge daemonsvc.MetricsGauge) {
	_, _ = fmt.Fprintf(out, "%s: current=%s min=%s mean=%s max=%s\n", name,
		formatMetricInt(gauge.Current), formatMetricInt(gauge.Min), formatMetricFloat(gauge.Mean), formatMetricInt(gauge.Max))
}

func writeMetricDuration(out io.Writer, label string, name string, duration daemonsvc.MetricsDuration) {
	nameField := ""
	if name != "" {
		nameField = " name=" + name
	}
	_, _ = fmt.Fprintf(out, "%s:%s total_ms=%s calls=%s mean_ms=%s p50_ms=%s p95_ms=%s max_ms=%s\n",
		label, nameField, formatMetricInt(duration.TotalMS), formatMetricInt(duration.Calls),
		formatMetricFloat(duration.MeanMS), formatMetricInt(duration.P50MS), formatMetricInt(duration.P95MS), formatMetricInt(duration.MaxMS))
}

func formatMetricInt(value *int64) string {
	if value == nil {
		return "n/a"
	}
	return strconv.FormatInt(*value, 10)
}

func formatMetricFloat(value *float64) string {
	if value == nil {
		return "n/a"
	}
	return strconv.FormatFloat(*value, 'f', 2, 64)
}

func joinPIDs(pids []int) string {
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, strconv.Itoa(pid))
	}
	return strings.Join(parts, ",")
}
