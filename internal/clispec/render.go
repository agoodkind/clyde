package clispec

import (
	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// RenderCobra builds every terminal command in the registry. An operation
// with a nil Group attaches at the root. Operations sharing the same *Group
// land as subcommands of one rendered parent, which itself sits at the root
// in the position of its first child. Hand-written commands follow at the
// root. The caller attaches the results to the root command, which already
// carries the global --verbose and --output-format flags.
func RenderCobra(reg *Registry, f *cli.Factory) []*cobra.Command {
	commands := make([]*cobra.Command, 0, len(reg.ops)+len(reg.handwritten))
	parents := map[*Group]*cobra.Command{}
	for _, op := range reg.ops {
		if !op.surfaceSet().CLI {
			continue
		}
		child := op.cobraCommand(f)
		groupRef := op.group()
		if groupRef == nil {
			commands = append(commands, child)
			continue
		}
		parent, ok := parents[groupRef]
		if !ok {
			parent = &cobra.Command{
				Use:   groupRef.Use,
				Short: groupRef.Short,
				Long:  groupRef.Long,
			}
			parents[groupRef] = parent
			commands = append(commands, parent)
		}
		parent.AddCommand(child)
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
