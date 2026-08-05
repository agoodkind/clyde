package status

import (
	"strconv"
	"strings"
	"time"
)

// statusMetric is one displayed fact: a dotted name, its display value, an
// optional unit, and the integer form when the value is a counter, which is
// what the terminal view diffs between refreshes.
type statusMetric struct {
	group    string
	name     string
	value    string
	unit     string
	intValue int64
	isInt    bool
}

func textMetric(group, name, value string) statusMetric {
	return statusMetric{group: group, name: name, value: value, unit: "", intValue: 0, isInt: false}
}

func intMetric(group, name string, value int64, unit string) statusMetric {
	return statusMetric{group: group, name: name, value: strconv.FormatInt(value, 10), unit: unit, intValue: value, isInt: true}
}

func boolMetric(group, name string, value bool) statusMetric {
	return textMetric(group, name, strconv.FormatBool(value))
}

// quotedError renders an error string the way the engine renders strings that
// could hold escapes: quoted, so a message cannot move the cursor.
func quotedError(group, name string, err error) statusMetric {
	return textMetric(group, name, strconv.Quote(err.Error()))
}

// buildMetrics flattens one snapshot into display order. Each section carries
// its own error metric so a dead surface stays visible beside the sections
// that answered.
func buildMetrics(snapshot statusSnapshot) []statusMetric {
	metrics := []statusMetric{
		boolMetric("daemon", "daemon.responding", snapshot.report.DaemonResponding),
		textMetric("daemon", "daemon.socket", snapshot.report.DaemonSocketPath),
		boolMetric("daemon", "daemon.socket_exists", snapshot.report.DaemonSocketExists),
	}
	if snapshot.report.DaemonError != "" {
		metrics = append(metrics, textMetric("daemon", "daemon.error", strconv.Quote(snapshot.report.DaemonError)))
	}
	metrics = append(metrics,
		boolMetric("daemon", "supervisor.responding", snapshot.report.SupervisorResponding),
		intMetric("daemon", "supervisor.pid", int64(snapshot.report.SupervisorPID), ""),
		textMetric("daemon", "supervisor.fingerprint", snapshot.report.SupervisorFingerprint),
	)
	if snapshot.report.SupervisorError != "" {
		metrics = append(metrics, textMetric("daemon", "supervisor.error", strconv.Quote(snapshot.report.SupervisorError)))
	}
	workerPids := make([]string, 0, len(snapshot.report.WorkerPIDs))
	for _, pid := range snapshot.report.WorkerPIDs {
		workerPids = append(workerPids, strconv.Itoa(pid))
	}
	metrics = append(metrics, textMetric("daemon", "worker.pids", strings.Join(workerPids, ",")))
	if snapshot.report.WorkerError != "" {
		metrics = append(metrics, textMetric("daemon", "worker.error", strconv.Quote(snapshot.report.WorkerError)))
	}
	if snapshot.report.LaunchdTarget != "" {
		metrics = append(metrics, textMetric("daemon", "launchd.target", snapshot.report.LaunchdTarget))
	}

	if snapshot.freshnessErr != nil {
		metrics = append(metrics, quotedError("freshness", "freshness.error", snapshot.freshnessErr))
	} else {
		lastSync := "null"
		if snapshot.freshness.LastSyncUnix > 0 {
			lastSync = time.Unix(snapshot.freshness.LastSyncUnix, 0).Format(time.RFC3339)
		}
		metrics = append(metrics,
			intMetric("freshness", "freshness.manifest", int64(snapshot.freshness.Manifest), "conversations"),
			intMetric("freshness", "freshness.needed", int64(snapshot.freshness.Needed), "conversations"),
			intMetric("freshness", "freshness.embedded", int64(snapshot.freshness.Embedded), "conversations"),
			intMetric("freshness", "freshness.pending", int64(snapshot.freshness.Pending), "conversations"),
			textMetric("freshness", "freshness.last_sync", lastSync),
		)
	}

	if snapshot.providersErr != nil {
		metrics = append(metrics, quotedError("providers", "providers.error", snapshot.providersErr))
	} else {
		for _, provider := range snapshot.providers.Providers {
			prefix := "provider." + provider.Provider.String() + "."
			metrics = append(metrics,
				intMetric("providers", prefix+"requests", int64(provider.Requests), "requests"),
				intMetric("providers", prefix+"inflight", int64(provider.Inflight), "requests"),
				intMetric("providers", prefix+"streaming", int64(provider.Streaming), "streams"),
				intMetric("providers", prefix+"input_tokens", provider.InputTokens, "tokens"),
				intMetric("providers", prefix+"output_tokens", provider.OutputTokens, "tokens"),
				intMetric("providers", prefix+"cache_read_tokens", provider.CacheReadTokens, "tokens"),
			)
			if provider.Error != "" {
				metrics = append(metrics, textMetric("providers", prefix+"error", strconv.Quote(provider.Error)))
			}
		}
	}

	if snapshot.mitmErr != nil {
		metrics = append(metrics, quotedError("mitm", "mitm.error", snapshot.mitmErr))
	} else {
		for _, listener := range snapshot.mitm.Listeners {
			prefix := "mitm." + listener.ID + "."
			metrics = append(metrics,
				textMetric("mitm", prefix+"address", listener.Address),
				boolMetric("mitm", prefix+"up", listener.Up),
			)
		}
	}
	return metrics
}

// renderPlainLines renders metrics as raw name value unit lines with a blank
// line between groups: the non-terminal output, and the values the terminal
// view aligns into columns.
func renderPlainLines(metrics []statusMetric) []string {
	lines := make([]string, 0, len(metrics)+4)
	previousGroup := ""
	for _, metric := range metrics {
		if previousGroup != "" && metric.group != previousGroup {
			lines = append(lines, "")
		}
		previousGroup = metric.group
		line := metric.name + " " + metric.value
		if metric.unit != "" {
			line += " " + metric.unit
		}
		lines = append(lines, line)
	}
	return lines
}
