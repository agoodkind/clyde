package mcpspec

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func uninstallMCPFile(ctx context.Context, client Client, path string, isJSON bool, homeDir string) (InstallFileResult, error) {
	slog.DebugContext(ctx, "mcp.settings.uninstall_file_started", "component", "mcpspec", "client", client, "path", path)
	result := InstallFileResult{Client: client, SettingsPath: path, Changed: false}
	if err := ctx.Err(); err != nil {
		slog.WarnContext(ctx, "mcp.settings.uninstall_file_failed", "component", "mcpspec", "client", client, "path", path, "err", err)
		return InstallFileResult{}, fmt.Errorf("uninstall %s MCP settings canceled: %w", client, err)
	}
	existing, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "mcp.settings.uninstall_file_failed", "component", "mcpspec", "client", client, "path", path, "err", err)
		return InstallFileResult{}, fmt.Errorf("read %s MCP settings %s: %w", client, path, err)
	}
	filtered := removeManagedClydeCodexTable(string(existing), homeDir)
	body := existing
	if filtered != string(existing) {
		body = []byte(strings.TrimSpace(filtered) + "\n")
	}
	if isJSON {
		body, err = uninstallJSONSettings(existing, homeDir)
		if err != nil {
			slog.WarnContext(ctx, "mcp.settings.uninstall_file_failed", "component", "mcpspec", "client", client, "path", path, "err", err)
			return InstallFileResult{}, fmt.Errorf("render %s MCP settings %s: %w", client, path, err)
		}
	}
	if bytes.Equal(existing, body) {
		return result, nil
	}
	// #nosec G703 -- path is one fixed settings path below the resolved home directory.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		slog.WarnContext(ctx, "mcp.settings.uninstall_file_failed", "component", "mcpspec", "client", client, "path", path, "err", err)
		return InstallFileResult{}, fmt.Errorf("write %s MCP settings %s: %w", client, path, err)
	}
	result.Changed = true
	return result, nil
}
