package mcpspec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type codexMCPKey string

const (
	codexMCPKeyCommand codexMCPKey = "command"
	codexMCPKeyArgs    codexMCPKey = "args"
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
	homeDir, err := installHomeDir(options.HomeDir)
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

func uninstallJSONSettings(existing []byte, homeDir string) ([]byte, error) {
	document := jsonDocument{}
	if err := json.Unmarshal(existing, &document); err != nil {
		slog.Warn("mcp.settings.uninstall_json_parse_failed", "err", err)
		return nil, fmt.Errorf("parse JSON settings: %w", err)
	}
	rawServers, ok := document["mcpServers"]
	if !ok {
		return existing, nil
	}
	servers := jsonServers{}
	if err := json.Unmarshal(rawServers, &servers); err != nil {
		slog.Warn("mcp.settings.uninstall_servers_parse_failed", "err", err)
		return nil, fmt.Errorf("parse mcpServers: %w", err)
	}
	rawClyde, ok := servers["clyde"]
	if !ok {
		return existing, nil
	}
	if !isManagedJSONServer(rawClyde, homeDir) {
		return existing, nil
	}
	delete(servers, "clyde")
	encodedServers, err := json.Marshal(servers)
	if err != nil {
		slog.Warn("mcp.settings.uninstall_servers_marshal_failed", "err", err)
		return nil, fmt.Errorf("marshal mcpServers: %w", err)
	}
	document["mcpServers"] = encodedServers
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		slog.Warn("mcp.settings.uninstall_json_marshal_failed", "err", err)
		return nil, fmt.Errorf("marshal JSON settings: %w", err)
	}
	return append(body, '\n'), nil
}

func isManagedJSONServer(raw []byte, homeDir string) bool {
	var server mcpServer
	if err := json.Unmarshal(raw, &server); err != nil {
		return false
	}
	return isManagedMCPServer(server, homeDir)
}

func removeManagedClydeCodexTable(input string, homeDir string) string {
	lines := strings.Split(input, "\n")
	output := make([]string, 0, len(lines))
	for index := 0; index < len(lines); {
		if !isClydeCodexHeader(lines[index]) {
			output = append(output, lines[index])
			index++
			continue
		}
		parentEnd := index + 1
		for parentEnd < len(lines) && !isTOMLTableHeader(lines[parentEnd]) {
			parentEnd++
		}
		end := parentEnd
		for end < len(lines) && isNestedClydeTable(lines[index], lines[end]) {
			nestedEnd := end + 1
			for nestedEnd < len(lines) && !isTOMLTableHeader(lines[nestedEnd]) {
				nestedEnd++
			}
			end = nestedEnd
		}
		if !isManagedMCPTable(lines[index:parentEnd], homeDir) {
			output = append(output, lines[index:end]...)
		}
		index = end
	}
	return strings.Join(output, "\n")
}

func isNestedClydeTable(parent string, candidate string) bool {
	parentHeader := strings.TrimSpace(parent)
	candidateHeader := strings.TrimSpace(candidate)
	if parentHeader == `[mcp_servers."clyde"]` {
		return strings.HasPrefix(candidateHeader, `[mcp_servers."clyde".`)
	}
	if parentHeader == "[mcp_servers.clyde]" {
		return strings.HasPrefix(candidateHeader, "[mcp_servers.clyde.")
	}
	return false
}

func isManagedMCPTable(lines []string, homeDir string) bool {
	server := mcpServer{Command: "", Args: nil}
	for _, line := range lines {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch codexMCPKey(strings.TrimSpace(key)) {
		case codexMCPKeyCommand:
			decoded, err := strconv.Unquote(strings.TrimSpace(value))
			if err == nil {
				server.Command = decoded
			}
		case codexMCPKeyArgs:
			if strings.Join(strings.Fields(value), "") == `["mcp","serve"]` {
				server.Args = []string{"mcp", "serve"}
			}
		default:
		}
	}
	return isManagedMCPServer(server, homeDir)
}

func isManagedMCPServer(server mcpServer, homeDir string) bool {
	command := filepath.Clean(server.Command)
	knownCommands := []string{
		filepath.Join(homeDir, ".local", "bin", "clyde"),
		"/usr/local/bin/clyde",
	}
	if executable, err := os.Executable(); err == nil {
		knownCommands = append(knownCommands, filepath.Clean(executable))
	}
	for _, knownCommand := range knownCommands {
		if command == filepath.Clean(knownCommand) {
			return len(server.Args) == 2 && server.Args[0] == "mcp" && server.Args[1] == "serve"
		}
	}
	return false
}
