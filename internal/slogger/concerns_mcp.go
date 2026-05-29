package slogger

// MCP concern constants.
const (
	ConcernMCPServerRequest = "mcp.server.requests"
	ConcernMCPServerContext = "mcp.server.context"
)

func init() {
	registerConcernPaths(map[string]string{
		ConcernMCPServerRequest: "mcp/server/requests.jsonl",
		ConcernMCPServerContext: "mcp/server/context.jsonl",
	})
}
