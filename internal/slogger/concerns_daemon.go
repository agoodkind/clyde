package slogger

// Daemon RPC and worker concern constants.
const (
	ConcernDaemonRPCRequests       = "daemon.rpc.requests"
	ConcernDaemonRPCStreams        = "daemon.rpc.streams"
	ConcernDaemonWorkersPrune      = "daemon.workers.prune"
	ConcernDaemonWorkersBridge     = "daemon.workers.bridge-watch"
	ConcernDaemonWorkersTranscript = "daemon.workers.transcript-hub"
	ConcernDaemonWorkersReload     = "daemon.workers.reload"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernDaemonRPCRequests:       "daemon/rpc/requests.jsonl",
		ConcernDaemonRPCStreams:        "daemon/rpc/streams.jsonl",
		ConcernDaemonWorkersPrune:      "daemon/workers/prune.jsonl",
		ConcernDaemonWorkersBridge:     "daemon/workers/bridge-watch.jsonl",
		ConcernDaemonWorkersTranscript: "daemon/workers/transcript-hub.jsonl",
		ConcernDaemonWorkersReload:     "daemon/workers/reload.jsonl",
	})

	registerEventConcernRules([]eventConcernRule{
		{"daemon.rpc.stream_", ConcernDaemonRPCStreams},
		{"daemon.rpc.", ConcernDaemonRPCRequests},
		{"daemon.reload.", ConcernDaemonWorkersReload},
		{"daemon.worker.reload", ConcernDaemonWorkersReload},
		{"daemon.bridge.", ConcernDaemonWorkersBridge},
		{"bridge.", ConcernDaemonWorkersBridge},
		{"transcript_hub.", ConcernDaemonWorkersTranscript},
		{"provider_stats.", ConcernDaemonWorkersTranscript},
		{"daemon.", ConcernProcessDaemonLifecycle},
		{"prune.autoname.", ConcernDaemonWorkersPrune},
		{"prune.delete.", ConcernDaemonWorkersPrune},
		{"prune.", ConcernDaemonWorkersPrune},
	})
}
