package mcpserver

// MCPMeta is the per-handler metadata stored alongside each
// livetrack.Session the MCP server registers. ServerName identifies
// the running MCP server instance. Method is the JSON-RPC method that
// dispatched the handler (e.g. "tools/call"). RequestID is the
// JSON-RPC request id for correlation. Tool names the specific tool
// being called, and is empty for non-tool-call handlers. Op
// classifies the operation kind so operators can filter snapshots
// without parsing Method.
//
// IsLivetrackMeta is the empty marker method the livetrack package
// requires; its only purpose is to constrain the registry's type
// parameter at compile time.
type MCPMeta struct {
	ServerName string
	Method     string
	RequestID  string
	Tool       string
	Op         string // "tool_call" | "sampling" | "list_roots" | "elicitation"
}

// IsLivetrackMeta is the empty marker method that satisfies the
// livetrack.Meta constraint. The body is intentionally empty.
func (MCPMeta) IsLivetrackMeta() {}
