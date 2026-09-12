package mcpspec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPreservesUnrelatedSettingsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, ".claude.json"), `{
  "theme": "dark",
  "mcpServers": {"other": {"command": "/bin/other"}}
}`)
	writeTestFile(t, filepath.Join(homeDir, ".cursor", "mcp.json"), `{
  "enabled": true,
  "mcpServers": {"other": {"command": "/bin/other"}}
}`)
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), `[features]
experimental = true

[mcp_servers.other]
command = "/bin/other"
`)

	installer := Installer{}
	options := InstallOptions{HomeDir: homeDir, ClydeBin: "/usr/local/bin/clyde"}
	first, err := installer.Install(context.Background(), options)
	if err != nil {
		t.Fatalf("first Install: %v", err)
	}
	second, err := installer.Install(context.Background(), options)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if !first.Changed {
		t.Fatal("first Changed = false, want true")
	}
	if second.Changed {
		t.Fatal("second Changed = true, want false")
	}
	if len(first.Files) != 3 {
		t.Fatalf("files len = %d, want 3", len(first.Files))
	}

	for _, path := range []string{
		filepath.Join(homeDir, ".claude.json"),
		filepath.Join(homeDir, ".cursor", "mcp.json"),
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		var document testJSONDocument
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatalf("Unmarshal %s: %v", path, err)
		}
		if string(document.Theme) != `"dark"` && string(document.Enabled) != "true" {
			t.Fatalf("unrelated JSON setting missing from %s: %s", path, body)
		}
		if _, ok := document.MCPServers["other"]; !ok {
			t.Fatalf("other MCP server missing from %s: %s", path, body)
		}
		clyde, ok := document.MCPServers["clyde"]
		if !ok {
			t.Fatalf("Clyde MCP server missing from %s: %s", path, body)
		}
		if clyde.Command != "/usr/local/bin/clyde" || strings.Join(clyde.Args, " ") != "mcp serve" {
			t.Fatalf("Clyde MCP server in %s = %#v", path, clyde)
		}
	}

	codexBody, err := os.ReadFile(filepath.Join(homeDir, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("ReadFile Codex config: %v", err)
	}
	for _, want := range []string{
		"experimental = true",
		"[mcp_servers.other]",
		`command = "/bin/other"`,
		"[mcp_servers.clyde]",
		`command = "/usr/local/bin/clyde"`,
		`args = ["mcp", "serve"]`,
	} {
		if !strings.Contains(string(codexBody), want) {
			t.Fatalf("Codex config missing %q:\n%s", want, codexBody)
		}
	}
}

type testJSONDocument struct {
	Theme      json.RawMessage          `json:"theme"`
	Enabled    json.RawMessage          `json:"enabled"`
	MCPServers map[string]testMCPServer `json:"mcpServers"`
}

type testMCPServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func writeTestFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}
