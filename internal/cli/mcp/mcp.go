package mcp

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/audit"
	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/mcpserver"
)

// NewCmd returns the `clyde mcp` command that starts the MCP stdio server.
func NewCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "mcp",
		Short:   "Start MCP stdio server for Claude Code integration",
		Long:    "Start the Model Context Protocol stdio server. The MCP client spawns this command as a subprocess and speaks the protocol over its standard input and output; the tool handlers call the daemon over gRPC.",
		Example: "clyde mcp",
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = f
			cliMCPLog.Logger().Info("cli.mcp.invoked", "concern",
				// Use NewServer so that Server.Tunnels (a *livetrack.Registry[MCPMeta])
				// is accessible from outside the mcpserver package. Holding the *Server
				// reference here matches the internal/mitm.Proxy.Tunnels pattern from
				// CLYDE-270, where the cross-package field access forces MCPMeta through
				// the public API and makes MCPMeta.IsLivetrackMeta reflection-reachable
				// to the deadcode analyzer.
				"cli.mcp")

			auditRotation := resolveAuditRotation()
			srv := mcpserver.NewServer(auditRotation)
			cliMCPLog.Logger().Info("cli.mcp.server_ready", "concern", "cli.mcp", "active_calls", srv.Tunnels.Count())
			return srv.Serve(cmd.Context())
		},
	}
}

// resolveAuditRotation pulls the audit sink rotation budget from the
// logpolicy boundary. On load or resolve failure the audit logger falls
// back to its own defaults; the MCP server still runs in that case.
func resolveAuditRotation() audit.RotationConfig {
	fallback := audit.RotationConfig{
		MaxSizeMB:  0,
		MaxBackups: 0,
		MaxAgeDays: 0,
		Compress:   false,
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return fallback
	}
	policies, err := logpolicy.Resolve(*cfg)
	if err != nil {
		return fallback
	}
	rotation := policies.Sinks[logpolicy.SinkAudit].Rotation
	return audit.RotationConfig{
		MaxSizeMB:  rotation.MaxSizeMB,
		MaxBackups: rotation.MaxBackups,
		MaxAgeDays: rotation.MaxAgeDays,
		Compress:   rotation.Compress,
	}
}
