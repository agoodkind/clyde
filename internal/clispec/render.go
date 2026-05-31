package clispec

import (
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// RenderCobra builds every terminal command in the registry: one command per
// operation whose surface set includes the terminal, followed by every
// hand-written command. The caller attaches the results to the root command,
// which already carries the global --verbose and --output-format flags.
func RenderCobra(reg *Registry, f *cli.Factory) []*cobra.Command {
	commands := make([]*cobra.Command, 0, len(reg.ops)+len(reg.handwritten))
	for _, op := range reg.ops {
		if op.surfaceSet().CLI {
			commands = append(commands, op.cobraCommand(f))
		}
	}
	for _, hand := range reg.handwritten {
		commands = append(commands, hand.Build(f))
	}
	return commands
}

// RenderMCP registers every MCP-exposed operation in the registry as a tool on
// the server. The server's existing middleware and tracing wrap each tool
// regardless of how the tool was built, so this function only adds tools.
func RenderMCP(reg *Registry, mcpServer *server.MCPServer) {
	for _, op := range reg.ops {
		if !op.surfaceSet().MCP {
			continue
		}
		tool, handler := op.mcpTool()
		mcpServer.AddTool(tool, handler)
	}
}
