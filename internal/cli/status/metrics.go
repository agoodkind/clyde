package status

import (
	"strconv"
	"strings"
	"time"

	lmstatus "goodkind.io/lm-semantic-search/status"
)

func textField(group, name, value string) lmstatus.Field {
	return lmstatus.Field{Group: group, Name: name, Unit: "", Value: lmstatus.Text(value), NoDelta: false}
}

func intField(group, name string, value int64, unit string) lmstatus.Field {
	return lmstatus.Field{Group: group, Name: name, Unit: unit, Value: lmstatus.Int(value), NoDelta: false}
}

func boolField(group, name string, value bool) lmstatus.Field {
	return lmstatus.Field{Group: group, Name: name, Unit: "", Value: lmstatus.Bool(value), NoDelta: false}
}

func buildSnapshot(snapshot statusSnapshot, build string) lmstatus.Snapshot {
	runID := ""
	if snapshot.report.SupervisorPID > 0 {
		runID = strconv.Itoa(snapshot.report.SupervisorPID)
	}
	return lmstatus.Snapshot{
		Title:    "clyde  version=" + build,
		Details:  nil,
		Notices:  nil,
		Identity: []lmstatus.Field{textField("status", "status.read_at", snapshot.readAt.UTC().Format(time.RFC3339))},
		RunID:    runID,
		Counters: buildFields(snapshot),
		Activity: nil,
	}
}

// A failed section becomes one error field, and the other sections stay.
func buildFields(snapshot statusSnapshot) []lmstatus.Field {
	fields := []lmstatus.Field{
		boolField("daemon", "daemon.responding", snapshot.report.DaemonResponding),
		textField("daemon", "daemon.socket", snapshot.report.DaemonSocketPath),
		boolField("daemon", "daemon.socket_exists", snapshot.report.DaemonSocketExists),
	}
	if snapshot.report.DaemonError != "" {
		fields = append(fields, textField("daemon", "daemon.error", snapshot.report.DaemonError))
	}
	supervisorPID := intField("daemon", "supervisor.pid", int64(snapshot.report.SupervisorPID), "")
	supervisorPID.NoDelta = true
	fields = append(fields,
		boolField("daemon", "supervisor.responding", snapshot.report.SupervisorResponding),
		supervisorPID,
		textField("daemon", "supervisor.fingerprint", snapshot.report.SupervisorFingerprint),
	)
	if snapshot.report.SupervisorError != "" {
		fields = append(fields, textField("daemon", "supervisor.error", snapshot.report.SupervisorError))
	}
	workerPids := make([]string, 0, len(snapshot.report.WorkerPIDs))
	for _, pid := range snapshot.report.WorkerPIDs {
		workerPids = append(workerPids, strconv.Itoa(pid))
	}
	fields = append(fields, textField("daemon", "worker.pids", strings.Join(workerPids, ",")))
	if snapshot.report.WorkerError != "" {
		fields = append(fields, textField("daemon", "worker.error", snapshot.report.WorkerError))
	}
	if snapshot.report.LaunchdTarget != "" {
		fields = append(fields, textField("daemon", "launchd.target", snapshot.report.LaunchdTarget))
	}
	fields = append(fields, semanticFields(snapshot.report.Runtime)...)

	if snapshot.freshnessErr != nil {
		fields = append(fields, textField("semantic_freshness", "semantic_freshness.error", snapshot.freshnessErr.Error()))
	} else {
		lastSync := lmstatus.Field{Group: "semantic_freshness", Name: "semantic_freshness.last_sync", Unit: "", Value: lmstatus.Value{}, NoDelta: false}
		if snapshot.freshness.LastSyncUnix > 0 {
			lastSync.Value = lmstatus.Text(time.Unix(snapshot.freshness.LastSyncUnix, 0).Format(time.RFC3339))
		}
		fields = append(fields,
			intField("semantic_freshness", "semantic_freshness.manifest", int64(snapshot.freshness.Manifest), "conversations"),
			intField("semantic_freshness", "semantic_freshness.needed", int64(snapshot.freshness.Needed), "conversations"),
			intField("semantic_freshness", "semantic_freshness.embedded", int64(snapshot.freshness.Embedded), "conversations"),
			intField("semantic_freshness", "semantic_freshness.pending", int64(snapshot.freshness.Pending), "conversations"),
			lastSync,
		)
	}

	if snapshot.providersErr != nil {
		fields = append(fields, textField("providers", "providers.error", snapshot.providersErr.Error()))
	} else {
		for _, provider := range snapshot.providers.Providers {
			prefix := "provider." + provider.Provider.String() + "."
			fields = append(fields,
				intField("providers", prefix+"requests", int64(provider.Requests), "requests"),
				intField("providers", prefix+"inflight", int64(provider.Inflight), "requests"),
				intField("providers", prefix+"streaming", int64(provider.Streaming), "streams"),
				intField("providers", prefix+"input_tokens", provider.InputTokens, "tokens"),
				intField("providers", prefix+"output_tokens", provider.OutputTokens, "tokens"),
				intField("providers", prefix+"cache_read_tokens", provider.CacheReadTokens, "tokens"),
			)
			if provider.Error != "" {
				fields = append(fields, textField("providers", prefix+"error", provider.Error))
			}
		}
	}

	if snapshot.mitmErr != nil {
		fields = append(fields, textField("mitm", "mitm.error", snapshot.mitmErr.Error()))
	} else {
		for _, listener := range snapshot.mitm.Listeners {
			prefix := "mitm." + listener.ID + "."
			fields = append(fields,
				textField("mitm", prefix+"address", listener.Address),
				boolField("mitm", prefix+"up", listener.Up),
			)
		}
	}
	return fields
}
