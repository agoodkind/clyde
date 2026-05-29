package slogger

// Livetrack concern constants. Covers the long-lived-work tracker
// the daemon, adapter, MITM, and MCP subsystems use to register
// HTTP connections, CONNECT tunnels, gRPC streams, provider
// websockets, SSE readers, MCP stdio handlers, file watchers, async
// workers, and capture-file flocks. Emits drain, force-close, and
// panic events.
const (
	ConcernLivetrack = "livetrack"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernLivetrack: "livetrack/livetrack.jsonl",
	})
}
