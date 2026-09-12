// Package mcpspec installs Clyde's MCP server registration in supported clients.
package mcpspec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Client identifies an MCP client settings format.
type Client string

const (
	ClientClaude Client = "claude"
	ClientCursor Client = "cursor"
	ClientCodex  Client = "codex"
)

// InstallOptions configures Clyde MCP registration.
type InstallOptions struct {
	HomeDir  string
	ClydeBin string
}

// InstallFileResult describes one MCP settings file.
type InstallFileResult struct {
	Client       Client
	SettingsPath string
	Changed      bool
}

// InstallResult describes all MCP settings changes.
type InstallResult struct {
	Files   []InstallFileResult
	Changed bool
}

// Installer registers Clyde without changing other MCP server entries.
type Installer struct{}

type mcpServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// jsonDocument keeps client-owned fields opaque while this package changes
// only the documented mcpServers.clyde boundary.
type jsonDocument map[string]json.RawMessage

type jsonServers map[string]json.RawMessage

// Install registers Clyde in Claude, Cursor, and Codex user settings.
func (Installer) Install(ctx context.Context, options InstallOptions) (InstallResult, error) {
	if err := ctx.Err(); err != nil {
		return InstallResult{}, fmt.Errorf("install MCP settings canceled: %w", err)
	}
	homeDir, err := installHomeDir(options.HomeDir)
	if err != nil {
		return InstallResult{}, err
	}
	clydeBin, err := installClydeBin(options.ClydeBin)
	if err != nil {
		return InstallResult{}, err
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
	result := InstallResult{Files: make([]InstallFileResult, 0, len(targets))}
	for _, target := range targets {
		fileResult, err := installSettingsFile(ctx, target.client, target.path, target.json, clydeBin)
		if err != nil {
			return InstallResult{}, err
		}
		result.Changed = result.Changed || fileResult.Changed
		result.Files = append(result.Files, fileResult)
	}
	return result, nil
}

func installSettingsFile(ctx context.Context, client Client, path string, isJSON bool, clydeBin string) (InstallFileResult, error) {
	if err := ctx.Err(); err != nil {
		return InstallFileResult{}, fmt.Errorf("install %s MCP settings canceled: %w", client, err)
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return InstallFileResult{}, fmt.Errorf("read %s MCP settings %s: %w", client, path, err)
	}
	var body []byte
	if isJSON {
		body, err = installJSONSettings(existing, clydeBin)
	} else {
		body = installCodexSettings(existing, clydeBin)
	}
	if err != nil {
		return InstallFileResult{}, fmt.Errorf("render %s MCP settings %s: %w", client, path, err)
	}
	changed := !bytes.Equal(bytes.TrimSpace(existing), bytes.TrimSpace(body))
	result := InstallFileResult{Client: client, SettingsPath: path, Changed: changed}
	if !changed {
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return InstallFileResult{}, fmt.Errorf("create %s MCP settings directory: %w", client, err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return InstallFileResult{}, fmt.Errorf("write %s MCP settings %s: %w", client, path, err)
	}
	return result, nil
}

func installJSONSettings(existing []byte, clydeBin string) ([]byte, error) {
	document := jsonDocument{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &document); err != nil {
			return nil, fmt.Errorf("parse JSON settings: %w", err)
		}
	}
	if document == nil {
		document = jsonDocument{}
	}
	servers := jsonServers{}
	if rawServers, ok := document["mcpServers"]; ok {
		if err := json.Unmarshal(rawServers, &servers); err != nil {
			return nil, fmt.Errorf("parse mcpServers: %w", err)
		}
	}
	if servers == nil {
		servers = jsonServers{}
	}
	clyde, err := json.Marshal(mcpServer{Command: clydeBin, Args: []string{"mcp", "serve"}})
	if err != nil {
		return nil, fmt.Errorf("marshal Clyde MCP server: %w", err)
	}
	servers["clyde"] = clyde
	encodedServers, err := json.Marshal(servers)
	if err != nil {
		return nil, fmt.Errorf("marshal mcpServers: %w", err)
	}
	document["mcpServers"] = encodedServers
	body, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal JSON settings: %w", err)
	}
	return append(body, '\n'), nil
}

func installCodexSettings(existing []byte, clydeBin string) []byte {
	base := strings.TrimSpace(removeClydeCodexTable(string(existing)))
	block := "[mcp_servers.clyde]\ncommand = " + strconv.Quote(clydeBin) + "\nargs = [\"mcp\", \"serve\"]"
	if base == "" {
		return []byte(block + "\n")
	}
	return []byte(base + "\n\n" + block + "\n")
}

func removeClydeCodexTable(input string) string {
	lines := strings.Split(input, "\n")
	output := make([]string, 0, len(lines))
	for index := 0; index < len(lines); {
		if !isClydeCodexHeader(lines[index]) {
			output = append(output, lines[index])
			index++
			continue
		}
		index++
		for index < len(lines) && !isTOMLTableHeader(lines[index]) {
			index++
		}
	}
	return strings.Join(output, "\n")
}

func isClydeCodexHeader(line string) bool {
	header := strings.TrimSpace(line)
	return header == "[mcp_servers.clyde]" ||
		strings.HasPrefix(header, "[mcp_servers.clyde.") ||
		header == `[mcp_servers."clyde"]` ||
		strings.HasPrefix(header, `[mcp_servers."clyde".`)
}

func isTOMLTableHeader(line string) bool {
	header := strings.TrimSpace(line)
	return strings.HasPrefix(header, "[") && strings.HasSuffix(header, "]")
}

func installHomeDir(value string) (string, error) {
	if homeDir := strings.TrimSpace(value); homeDir != "" {
		return homeDir, nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if strings.TrimSpace(homeDir) == "" {
		return "", fmt.Errorf("resolve home directory: empty path")
	}
	return homeDir, nil
}

func installClydeBin(value string) (string, error) {
	if clydeBin := strings.TrimSpace(value); clydeBin != "" {
		return clydeBin, nil
	}
	clydeBin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve Clyde executable: %w", err)
	}
	return clydeBin, nil
}
