package slogger

// Daemon RPC and worker concern constants.
const (
	ConcernDaemonWorkersReload = "daemon.workers.reload"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernDaemonWorkersReload: "daemon/workers/reload.jsonl",
	})
}
