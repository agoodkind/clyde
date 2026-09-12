package mcpspec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallCodexUsesManagedParentFieldsOnly(t *testing.T) {
	homeDir := t.TempDir()
	path := filepath.Join(homeDir, ".codex", "config.toml")
	writeTestFile(t, path, `[mcp_servers.clyde]
command = "/usr/local/bin/clyde"
args = ["mcp", "serve"]

[mcp_servers.clyde.env]
command = "/bin/foreign"

[mcp_servers.other]
command = "/bin/other"
`)

	if _, err := (Uninstaller{}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(body) == "" || containsAny(string(body), "[mcp_servers.clyde]", "[mcp_servers.clyde.env]") {
		t.Fatalf("managed Codex tables survived:\n%s", body)
	}
	if !containsAny(string(body), "[mcp_servers.other]", `command = "/bin/other"`) {
		t.Fatalf("unrelated Codex table was removed:\n%s", body)
	}
}

func TestUninstallPreservesForeignClydeExecutable(t *testing.T) {
	homeDir := t.TempDir()
	codexPath := filepath.Join(homeDir, ".codex", "config.toml")
	codexBody := `[mcp_servers.clyde]
command = "/opt/other/clyde"
args = ["mcp", "serve"]
`
	writeTestFile(t, codexPath, codexBody)
	claudePath := filepath.Join(homeDir, ".claude.json")
	claudeBody := `{"mcpServers":{"clyde":{"command":"/opt/other/clyde","args":["mcp","serve"]}}}`
	writeTestFile(t, claudePath, claudeBody)

	result, err := (Uninstaller{}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if result.Changed {
		t.Fatal("foreign MCP registrations were removed")
	}
	gotCodex, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("ReadFile Codex: %v", err)
	}
	if string(gotCodex) != codexBody {
		t.Fatalf("foreign Codex registration changed:\n%s", gotCodex)
	}
	gotClaude, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("ReadFile Claude: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(gotClaude, &document); err != nil {
		t.Fatalf("Unmarshal Claude: %v", err)
	}
	if _, ok := document["mcpServers"]; !ok {
		t.Fatal("foreign Claude MCP registration was removed")
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
