package mcpspec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallRemovesOnlyClydeMCPEntries(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, ".claude.json"), `{"theme":"dark","mcpServers":{"other":{"command":"/bin/other"},"clyde":{"command":"/usr/local/bin/clyde","args":["mcp","serve"]}}}`)
	writeTestFile(t, filepath.Join(homeDir, ".cursor", "mcp.json"), `{"enabled":true,"mcpServers":{"other":{"command":"/bin/other"},"clyde":{"command":"/usr/local/bin/clyde","args":["mcp","serve"]}}}`)
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), "[features]\nexperimental = true\n\n[mcp_servers.other]\ncommand = \"/bin/other\"\n\n[mcp_servers.clyde]\ncommand = \"/usr/local/bin/clyde\"\nargs = [\"mcp\", \"serve\"]\n")
	result, err := (Uninstaller{}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !result.Changed {
		t.Fatal("Changed = false, want true")
	}
	for _, path := range []string{filepath.Join(homeDir, ".claude.json"), filepath.Join(homeDir, ".cursor", "mcp.json")} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		var document testJSONDocument
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatalf("Unmarshal %s: %v", path, err)
		}
		if _, ok := document.MCPServers["clyde"]; ok {
			t.Fatalf("Clyde MCP entry remains in %s", path)
		}
		if _, ok := document.MCPServers["other"]; !ok {
			t.Fatalf("unrelated MCP entry removed from %s", path)
		}
	}
	codexBody, err := os.ReadFile(filepath.Join(homeDir, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(codexBody), "mcp_servers.clyde") || !strings.Contains(string(codexBody), "mcp_servers.other") || !strings.Contains(string(codexBody), "experimental = true") {
		t.Fatalf("Codex MCP settings changed incorrectly:\n%s", codexBody)
	}
	second, err := (Uninstaller{}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir})
	if err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
	if second.Changed {
		t.Fatal("second uninstall changed unrelated settings")
	}
}

func TestUninstallPreservesForeignMCPEntryNamedClyde(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, ".claude.json"), `{"mcpServers":{"clyde":{"command":"/opt/other/clyde","args":["mcp","serve"]}}}`)
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), "[mcp_servers.clyde]\ncommand = \"/opt/other/clyde\"\nargs = [\"mcp\", \"serve\"]\n")

	result, err := (Uninstaller{}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if result.Changed {
		t.Fatal("foreign Clyde entries were changed")
	}
	for _, path := range []string{filepath.Join(homeDir, ".claude.json"), filepath.Join(homeDir, ".codex", "config.toml")} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "/opt/other/clyde") {
			t.Fatalf("foreign Clyde entry removed from %s", path)
		}
	}
}

func TestUninstallRemovesManagedCodexSubtables(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), "[mcp_servers.clyde]\ncommand = \"/usr/local/bin/clyde\"\nargs = [\"mcp\", \"serve\"]\n\n[mcp_servers.clyde.env]\nTOKEN = \"value\"\ncommand = \"/opt/other/clyde\"\n\n[mcp_servers.other]\ncommand = \"/bin/other\"\n")

	if _, err := (Uninstaller{}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(homeDir, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "mcp_servers.clyde") || strings.Contains(string(body), "TOKEN =") {
		t.Fatalf("managed Clyde subtables remain:\n%s", body)
	}
	if !strings.Contains(string(body), "mcp_servers.other") {
		t.Fatalf("unrelated Codex MCP entry removed:\n%s", body)
	}
}
