package hookspec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type testClaudeSettings struct {
	Theme string                           `json:"theme,omitempty"`
	Hooks map[string][]testClaudeHookGroup `json:"hooks,omitempty"`
}

type testClaudeHookGroup struct {
	Matcher string                  `json:"matcher,omitempty"`
	Hooks   []testClaudeHookHandler `json:"hooks"`
}

type testClaudeHookHandler struct {
	Type          string   `json:"type"`
	Command       string   `json:"command"`
	Args          []string `json:"args,omitempty"`
	Timeout       int      `json:"timeout,omitempty"`
	StatusMessage string   `json:"statusMessage,omitempty"`
}

type testRawClaudeSettings struct {
	Hooks map[string][]testRawClaudeHookGroup `json:"hooks,omitempty"`
}

type testRawClaudeHookGroup struct {
	Matcher string          `json:"matcher,omitempty"`
	Hooks   json.RawMessage `json:"hooks"`
}

func TestInstallerCreatesUserClaudeSettings(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installer := Installer{Registry: NewRegistry()}
	result, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientClaudeCode,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	expectedPath := filepath.Join(homeDir, ".claude", "settings.json")
	if result.SettingsPath != expectedPath {
		t.Fatalf("settings path = %q, want %q", result.SettingsPath, expectedPath)
	}
	if !result.Changed {
		t.Fatal("Changed = false, want true")
	}
	if !slices.Equal(result.Installed, []HookID{HookIDClaudeCodeReorientAfterCompact}) {
		t.Fatalf("installed = %#v", result.Installed)
	}

	settings := readTestClaudeSettings(t, expectedPath)
	groups := settings.Hooks[string(ClaudeCodeEventSessionStart)]
	handler := findTestHookHandler(t, groups, "compact")
	if handler.Type != "command" {
		t.Fatalf("type = %q, want command", handler.Type)
	}
	if handler.Command != "/usr/local/bin/clyde" {
		t.Fatalf("command = %q", handler.Command)
	}
	expectedArgs := []string{"hooks", "run", string(HookIDClaudeCodeReorientAfterCompact)}
	if !slices.Equal(handler.Args, expectedArgs) {
		t.Fatalf("args = %#v, want %#v", handler.Args, expectedArgs)
	}
	if handler.Timeout != 600 {
		t.Fatalf("timeout = %d, want 600", handler.Timeout)
	}
}

func TestInstallerIsIdempotent(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installer := Installer{Registry: NewRegistry()}
	options := InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientClaudeCode,
	}

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
}

func TestInstallerPreservesUnrelatedSettingsAndHooks(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	initial := []byte(`{
  "theme": "dark",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "/bin/echo",
            "args": ["hello"]
          }
        ]
      }
    ]
  }
}`)
	if err := os.WriteFile(settingsPath, initial, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	installer := Installer{Registry: NewRegistry()}
	_, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientClaudeCode,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	settings := readTestClaudeSettings(t, settingsPath)
	if settings.Theme != "dark" {
		t.Fatalf("theme = %q, want dark", settings.Theme)
	}
	groups := settings.Hooks[string(ClaudeCodeEventSessionStart)]
	_ = findTestHookHandler(t, groups, "startup")
	_ = findTestHookHandler(t, groups, "compact")
}

func TestInstallerDryRunWritesNoFiles(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	installer := Installer{Registry: NewRegistry()}
	result, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientClaudeCode,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if !result.DryRun {
		t.Fatal("DryRun = false, want true")
	}
	if len(result.Preview) == 0 {
		t.Fatal("Preview was empty")
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("settings file exists after dry run, stat err = %v", err)
	}
}

func TestInstallerDoesNotOverwriteMalformedExistingHookGroup(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	initial := []byte(`{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "compact",
        "hooks": {
          "unexpected": true
        }
      }
    ]
  }
}`)
	if err := os.WriteFile(settingsPath, initial, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	installer := Installer{Registry: NewRegistry()}
	_, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientClaudeCode,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var settings testRawClaudeSettings
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("Unmarshal settings: %v\n%s", err, string(body))
	}
	groups := settings.Hooks[string(ClaudeCodeEventSessionStart)]
	if len(groups) != 2 {
		t.Fatalf("SessionStart groups len = %d, want 2", len(groups))
	}
	var preserved map[string]json.RawMessage
	if err := json.Unmarshal(groups[0].Hooks, &preserved); err != nil {
		t.Fatalf("Unmarshal preserved hooks: %v", err)
	}
	if _, ok := preserved["unexpected"]; !ok {
		t.Fatalf("preserved hooks = %#v, want unexpected key", preserved)
	}
	var handlers []testClaudeHookHandler
	if err := json.Unmarshal(groups[1].Hooks, &handlers); err != nil {
		t.Fatalf("Unmarshal added handlers: %v", err)
	}
	if len(handlers) != 1 {
		t.Fatalf("added handlers len = %d, want 1", len(handlers))
	}
	if handlers[0].Command != "/usr/local/bin/clyde" {
		t.Fatalf("added command = %q", handlers[0].Command)
	}
}

func readTestClaudeSettings(t *testing.T, path string) testClaudeSettings {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var settings testClaudeSettings
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("Unmarshal settings: %v\n%s", err, string(body))
	}
	return settings
}

func findTestHookHandler(
	t *testing.T,
	groups []testClaudeHookGroup,
	matcher string,
) testClaudeHookHandler {
	t.Helper()

	for _, group := range groups {
		if group.Matcher != matcher {
			continue
		}
		if len(group.Hooks) == 0 {
			t.Fatalf("group %q had no hooks", matcher)
		}
		return group.Hooks[0]
	}
	t.Fatalf("missing hook group matcher %q", matcher)
	return testClaudeHookHandler{}
}
