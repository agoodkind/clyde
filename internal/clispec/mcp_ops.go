package clispec

import (
	"context"

	"goodkind.io/clyde/internal/audit"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logpolicy"
	"goodkind.io/clyde/internal/mcpserver"
)

type mcpServeInput struct{}

func (mcpServeInput) isClispecInput() {}

type mcpServePayload struct{}

func (mcpServePayload) isClispecPrepared() {}

var mcpGroup = &Group{
	Use:     "mcp",
	Short:   "Manage MCP server commands",
	Long:    "Manage the Model Context Protocol stdio server commands.",
	Example: "clyde mcp serve",
	Parent:  nil,
}

func mcpServeOp() Operation[mcpServeInput, mcpServePayload] {
	return Operation[mcpServeInput, mcpServePayload]{
		Name:           Name{Canonical: "mcp_serve", CLIOverride: "serve"},
		Group:          mcpGroup,
		Surfaces:       SurfaceSet{CLI: true, MCP: false},
		outputKind:     0,
		Short:          "Start MCP stdio server for Claude Code integration",
		Long:           "Start the Model Context Protocol stdio server. The MCP client spawns this command as a subprocess and speaks the protocol over its standard input and output; the tool handlers call the daemon over gRPC.",
		Examples:       []string{"clyde mcp serve"},
		Args:           nil,
		Params:         nil,
		New:            func() mcpServeInput { return mcpServeInput{} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare:        func(in mcpServeInput) (mcpServePayload, error) { return mcpServePayload{}, nil },
		Run: func(ctx context.Context, _ mcpServePayload, _ Surface, _ ResultSink) error {
			srv := mcpserver.NewServer(resolveMCPAuditRotation())
			RenderMCP(NewConversationRegistry(), srv.MCPServer())
			return srv.Serve(ctx)
		},
		runResult: nil,
	}
}

func resolveMCPAuditRotation() audit.RotationConfig {
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
