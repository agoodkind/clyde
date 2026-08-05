package status

import (
	"goodkind.io/clyde/internal/daemon"
)

// statusJSON is the JSON snapshot the status command emits under
// --output-format json. Section errors render as strings so a dead surface
// stays visible beside the sections that answered.
type statusJSON struct {
	ReadAt    string             `json:"read_at"`
	Daemon    daemonJSON         `json:"daemon"`
	Freshness *freshnessJSON     `json:"freshness,omitempty"`
	Providers []providerJSON     `json:"providers,omitempty"`
	MITM      []mitmListenerJSON `json:"mitm_listeners,omitempty"`
	Errors    sectionErrorsJSON  `json:"errors"`
}

type daemonJSON struct {
	Responding            bool   `json:"responding"`
	SocketPath            string `json:"socket_path"`
	SocketExists          bool   `json:"socket_exists"`
	SupervisorResponding  bool   `json:"supervisor_responding"`
	SupervisorPID         int    `json:"supervisor_pid"`
	SupervisorFingerprint string `json:"supervisor_fingerprint"`
	WorkerPIDs            []int  `json:"worker_pids,omitempty"`
	LaunchdTarget         string `json:"launchd_target,omitempty"`
}

type freshnessJSON struct {
	Manifest     int   `json:"manifest"`
	Needed       int   `json:"needed"`
	Embedded     int   `json:"embedded"`
	Pending      int   `json:"pending"`
	LastSyncUnix int64 `json:"last_sync_unix"`
}

type providerJSON struct {
	Provider        string `json:"provider"`
	Requests        int    `json:"requests"`
	Inflight        int    `json:"inflight"`
	Streaming       int    `json:"streaming"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	Error           string `json:"error,omitempty"`
}

type mitmListenerJSON struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Up      bool   `json:"up"`
}

type sectionErrorsJSON struct {
	Daemon     string `json:"daemon,omitempty"`
	Supervisor string `json:"supervisor,omitempty"`
	Worker     string `json:"worker,omitempty"`
	Freshness  string `json:"freshness,omitempty"`
	Providers  string `json:"providers,omitempty"`
	MITM       string `json:"mitm,omitempty"`
}

// snapshotOutput maps one gathered snapshot onto its JSON form.
func snapshotOutput(snapshot statusSnapshot) statusJSON {
	out := statusJSON{
		ReadAt: snapshot.readAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Daemon: daemonJSON{
			Responding:            snapshot.report.DaemonResponding,
			SocketPath:            snapshot.report.DaemonSocketPath,
			SocketExists:          snapshot.report.DaemonSocketExists,
			SupervisorResponding:  snapshot.report.SupervisorResponding,
			SupervisorPID:         snapshot.report.SupervisorPID,
			SupervisorFingerprint: snapshot.report.SupervisorFingerprint,
			WorkerPIDs:            snapshot.report.WorkerPIDs,
			LaunchdTarget:         snapshot.report.LaunchdTarget,
		},
		Freshness: nil,
		Providers: nil,
		MITM:      nil,
		Errors: sectionErrorsJSON{
			Daemon:     snapshot.report.DaemonError,
			Supervisor: snapshot.report.SupervisorError,
			Worker:     snapshot.report.WorkerError,
			Freshness:  "",
			Providers:  "",
			MITM:       "",
		},
	}
	if snapshot.freshnessErr != nil {
		out.Errors.Freshness = snapshot.freshnessErr.Error()
	} else {
		out.Freshness = &freshnessJSON{
			Manifest:     snapshot.freshness.Manifest,
			Needed:       snapshot.freshness.Needed,
			Embedded:     snapshot.freshness.Embedded,
			Pending:      snapshot.freshness.Pending,
			LastSyncUnix: snapshot.freshness.LastSyncUnix,
		}
	}
	if snapshot.providersErr != nil {
		out.Errors.Providers = snapshot.providersErr.Error()
	} else {
		out.Providers = providerOutputs(snapshot.providers)
	}
	if snapshot.mitmErr != nil {
		out.Errors.MITM = snapshot.mitmErr.Error()
	} else {
		for _, listener := range snapshot.mitm.Listeners {
			out.MITM = append(out.MITM, mitmListenerJSON{ID: listener.ID, Address: listener.Address, Up: listener.Up})
		}
	}
	return out
}

func providerOutputs(snapshot daemon.ProviderStatsSnapshot) []providerJSON {
	outputs := make([]providerJSON, 0, len(snapshot.Providers))
	for _, provider := range snapshot.Providers {
		outputs = append(outputs, providerJSON{
			Provider:        provider.Provider.String(),
			Requests:        provider.Requests,
			Inflight:        provider.Inflight,
			Streaming:       provider.Streaming,
			InputTokens:     provider.InputTokens,
			OutputTokens:    provider.OutputTokens,
			CacheReadTokens: provider.CacheReadTokens,
			Error:           provider.Error,
		})
	}
	return outputs
}
