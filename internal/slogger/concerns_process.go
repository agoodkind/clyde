package slogger

// Process-daemon and cmd concern constants.
const (
	ConcernProcessDaemonLifecycle = "process.daemon.lifecycle"
	ConcernProcessDaemonLocks     = "process.daemon.locks"
	ConcernProcessDaemonListeners = "process.daemon.listeners"
	ConcernProcessDaemonConfig    = "process.daemon.config"

	ConcernCmdDispatch = "cmd.dispatch"
	ConcernCmdResume   = "cmd.resume"
	ConcernCmdCompact  = "cmd.compact"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernProcessDaemonLifecycle: "process/daemon/lifecycle.jsonl",
		ConcernProcessDaemonLocks:     "process/daemon/locks.jsonl",
		ConcernProcessDaemonListeners: "process/daemon/listeners.jsonl",
		ConcernProcessDaemonConfig:    "process/daemon/config.jsonl",
		ConcernCmdDispatch:            "cmd/dispatch.jsonl",
		ConcernCmdResume:              "cmd/resume.jsonl",
		ConcernCmdCompact:             "cmd/compact.jsonl",
	})

	registerEventConcernRules([]eventConcernRule{
		{"cli.args.", ConcernCmdDispatch},
		{"cli.execute.", ConcernCmdDispatch},
		{"cli.main.", ConcernCmdDispatch},
		{"cli.resume.", ConcernCmdResume},
		{"cmd.session.", ConcernCmdResume},
		{"forward.", ConcernCmdDispatch},
		{"adapter listening", ConcernProcessDaemonListeners},
	})
}
