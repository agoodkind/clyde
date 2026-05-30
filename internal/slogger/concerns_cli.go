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
