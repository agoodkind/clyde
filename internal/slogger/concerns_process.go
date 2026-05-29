package slogger

// Process-daemon concern constants.
const (
	ConcernProcessDaemonLifecycle = "process.daemon.lifecycle"
	ConcernProcessDaemonListeners = "process.daemon.listeners"
	ConcernProcessDaemonConfig    = "process.daemon.config"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernProcessDaemonLifecycle: "process/daemon/lifecycle.jsonl",
		ConcernProcessDaemonListeners: "process/daemon/listeners.jsonl",
		ConcernProcessDaemonConfig:    "process/daemon/config.jsonl",
	})
}
