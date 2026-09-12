package mcpspec

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

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
