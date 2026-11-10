package mcpspec

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
)

// UninstallOptions configures Clyde MCP registration removal.
type UninstallOptions struct {
	HomeDir string
}

// UninstallResult describes all MCP settings changes.
type UninstallResult struct {
	Files   []InstallFileResult
	Changed bool
}

// Uninstaller removes Clyde's MCP registration from supported clients.
type Uninstaller struct{}

// Uninstall removes only the Clyde MCP entry from supported client settings.
func (Uninstaller) Uninstall(ctx context.Context, options UninstallOptions) (UninstallResult, error) {
	slog.InfoContext(ctx, "mcp.settings.uninstall_started", "component", "mcpspec")
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "mcp.settings.uninstall_failed", "component", "mcpspec", "err", err)
		return UninstallResult{}, fmt.Errorf("uninstall MCP settings canceled: %w", err)
	}
	homeDir, err := installHomeDir(ctx, options.HomeDir)
	if err != nil {
		slog.WarnContext(ctx, "mcp.settings.uninstall_failed", "component", "mcpspec", "err", err)
		return UninstallResult{}, err
	}
	targets := []struct {
		client Client
		path   string
		json   bool
	}{
		{client: ClientClaude, path: filepath.Join(homeDir, ".claude.json"), json: true},
		{client: ClientCursor, path: filepath.Join(homeDir, ".cursor", "mcp.json"), json: true},
		{client: ClientCodex, path: filepath.Join(homeDir, ".codex", "config.toml"), json: false},
	}
	result := UninstallResult{Files: make([]InstallFileResult, 0, len(targets)), Changed: false}
	for _, target := range targets {
		fileResult, err := uninstallMCPFile(ctx, target.client, target.path, target.json, homeDir)
		if err != nil {
			slog.WarnContext(ctx, "mcp.settings.uninstall_failed", "component", "mcpspec", "client", target.client, "err", err)
			return UninstallResult{}, err
		}
		result.Changed = result.Changed || fileResult.Changed
		result.Files = append(result.Files, fileResult)
	}
	slog.InfoContext(ctx, "mcp.settings.uninstall_completed", "component", "mcpspec", "changed", result.Changed)
	return result, nil
}
