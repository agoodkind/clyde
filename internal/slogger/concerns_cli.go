package slogger

// CLI subsystem concern constants. Covers the user-facing commands
// outside the daemon (clyde logs inventory, clyde mcp server, clyde
// mitm trust/show/status, clyde conversation export, clyde daemon
// invocation).
const (
	ConcernCLIDaemon         = "cli.daemon"
	ConcernCLIMCP            = "cli.mcp"
	ConcernCLILogs           = "cli.logs"
	ConcernCLIMITM           = "cli.mitm"
	ConcernCLIMITMTruststore = "cli.mitm.truststore"
	ConcernCLIConversation   = "cli.conversation"
	ConcernCLIOutput         = "cli.output"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernCLIDaemon:         "cli/daemon.jsonl",
		ConcernCLIMCP:            "cli/mcp.jsonl",
		ConcernCLILogs:           "cli/logs.jsonl",
		ConcernCLIMITM:           "cli/mitm.jsonl",
		ConcernCLIMITMTruststore: "cli/mitm/truststore.jsonl",
		ConcernCLIConversation:   "cli/conversation.jsonl",
		ConcernCLIOutput:         "cli/output.jsonl",
	})
}
