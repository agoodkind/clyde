package mcp

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/mcpserver"
)

// NewCmd returns the hidden `clyde mcp` command that starts the MCP stdio server.
func NewCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Start MCP stdio server for Claude Code integration",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = f
			cliMCPLog.Logger().Info("cli.mcp.invoked")
			// Use NewServer so that Server.Tunnels (a *livetrack.Registry[MCPMeta])
			// is accessible from outside the mcpserver package. Holding the *Server
			// reference here matches the internal/mitm.Proxy.Tunnels pattern from
			// CLYDE-270, where the cross-package field access forces MCPMeta through
			// the public API and makes MCPMeta.IsLivetrackMeta reflection-reachable
			// to the deadcode analyzer.
			srv := mcpserver.NewServer()
			cliMCPLog.Logger().Info("cli.mcp.server_ready",
				"active_calls", srv.Tunnels.Count(),
			)
			return srv.Serve(cmd.Context())
		},
	}
}
