package hookspec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

type testCursorHooks struct {
	Version int                                   `json:"version"`
	Extra   bool                                  `json:"extra,omitempty"`
	Hooks   map[string][]testCursorHookDefinition `json:"hooks"`
}

type testCursorHookDefinition struct {
	Command    string `json:"command"`
	Timeout    int    `json:"timeout"`
	Matcher    string `json:"matcher,omitempty"`
	FailClosed bool   `json:"failClosed"`
	LoopLimit  int    `json:"loop_limit,omitempty"`
}

func TestInstallerCreatesUserHookSettingsForAllClients(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installer := Installer{Registry: NewRegistry()}
	result, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.Changed {
		t.Fatal("Changed = false, want true")
	}
	if result.DryRun {
		t.Fatal("DryRun = true, want false")
	}

	claudePath := filepath.Join(homeDir, ".claude", "settings.json")
	codexPath := filepath.Join(homeDir, ".codex", "config.toml")
	cursorPath := filepath.Join(homeDir, ".cursor", "hooks.json")
	assertInstallFile(t, result, ClientClaudeCode, claudePath, []HookID{HookIDReorientBeforeCompact, HookIDReorientAfterCompact})
	assertInstallFile(t, result, ClientCodex, codexPath, []HookID{HookIDReorientBeforeCompact, HookIDReorientAfterCompact})
	assertInstallFile(t, result, ClientCursor, cursorPath, []HookID{HookIDReorientBeforeCompact, HookIDReorientStopFollowup})

	claude := readTestClaudeSettings(t, claudePath)
	assertClaudeHandler(t, claude, EventPreCompact, "", "/usr/local/bin/clyde", []string{"hooks", "run", "reorient-before-compact"}, 600)
	assertClaudeHandler(t, claude, EventSessionStart, "compact", "/usr/local/bin/clyde", []string{"hooks", "run", "reorient-after-compact"}, 600)

	codexBody := readTextFile(t, codexPath)
	for _, want := range []string{
		"[features]",
		"hooks = true",
		"[[hooks.pre_compact]]",
		"[[hooks.session_start]]",
		"matcher = \"compact\"",
		"command = \"/usr/local/bin/clyde hooks run reorient-before-compact\"",
		"command = \"/usr/local/bin/clyde hooks run reorient-after-compact\"",
		"trusted_hash = \"sha256:",
	} {
		if !strings.Contains(codexBody, want) {
			t.Fatalf("Codex config missing %q:\n%s", want, codexBody)
		}
	}

	cursor := readTestCursorHooks(t, cursorPath)
	if cursor.Version != 1 {
		t.Fatalf("Cursor version = %d, want 1", cursor.Version)
	}
	assertCursorCommand(t, cursor, EventCursorPre, "/usr/local/bin/clyde hooks run reorient-before-compact", 0)
	assertCursorCommand(t, cursor, EventCursorStop, "/usr/local/bin/clyde hooks run reorient-stop-followup", 1)
}

func TestInstallerIsIdempotent(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installer := Installer{Registry: NewRegistry()}
	options := InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientAll,
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
	writeTestFile(t, filepath.Join(homeDir, ".claude", "settings.json"), `{
  "theme": "dark",
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {"type": "command", "command": "/bin/echo", "args": ["hello"]}
        ]
      }
    ]
  }
}`)
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), `[features]
experimental = true

approval_policy = "never"

[[hooks.PreToolUse]]
matcher = "Bash"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "/bin/echo old"
`)
	writeTestFile(t, filepath.Join(homeDir, ".cursor", "hooks.json"), `{
  "version": 1,
  "extra": true,
  "hooks": {
    "preToolUse": [{"command": "/bin/echo old", "timeout": 1}]
  }
}`)

	installer := Installer{Registry: NewRegistry()}
	_, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientAll,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	claude := readTestClaudeSettings(t, filepath.Join(homeDir, ".claude", "settings.json"))
	if claude.Theme != "dark" {
		t.Fatalf("theme = %q, want dark", claude.Theme)
	}
	assertClaudeHandler(t, claude, EventSessionStart, "startup", "/bin/echo", []string{"hello"}, 0)
	assertClaudeHandler(t, claude, EventSessionStart, "compact", "/usr/local/bin/clyde", []string{"hooks", "run", "reorient-after-compact"}, 600)

	codexBody := readTextFile(t, filepath.Join(homeDir, ".codex", "config.toml"))
	for _, want := range []string{"experimental = true", "approval_policy = \"never\"", "command = \"/bin/echo old\""} {
		if !strings.Contains(codexBody, want) {
			t.Fatalf("Codex config missing preserved %q:\n%s", want, codexBody)
		}
	}

	cursor := readTestCursorHooks(t, filepath.Join(homeDir, ".cursor", "hooks.json"))
	if !cursor.Extra {
		t.Fatal("Cursor extra field was not preserved")
	}
	assertCursorCommand(t, cursor, "preToolUse", "/bin/echo old", 0)
}

func TestInstallerReplacesOldClydeSignatures(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, ".claude", "settings.json"), `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "compact",
        "hooks": [
          {"type": "command", "command": "/old/clyde", "args": ["hooks", "run", "claude-code-reorient-after-compact"]}
        ]
      }
    ]
  }
}`)
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), `[[hooks.SessionStart]]
matcher = "compact"

[[hooks.SessionStart.hooks]]
type = "command"
command = "/usr/local/bin/clyde hooks run claude-code-reorient-after-compact"
`)
	writeTestFile(t, filepath.Join(homeDir, ".cursor", "hooks.json"), `{
  "version": 1,
  "hooks": {
    "stop": [{"command": "/usr/local/bin/clyde hooks run reorient-stop-followup", "timeout": 10}]
  }
}`)

	installer := Installer{Registry: NewRegistry()}
	_, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientAll,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if strings.Contains(readTextFile(t, filepath.Join(homeDir, ".claude", "settings.json")), "claude-code-reorient-after-compact") {
		t.Fatal("legacy Claude hook signature was preserved")
	}
	claude := readTestClaudeSettings(t, filepath.Join(homeDir, ".claude", "settings.json"))
	assertClaudeHandler(t, claude, EventSessionStart, "compact", "/usr/local/bin/clyde", []string{"hooks", "run", "reorient-after-compact"}, 600)
	codexBody := readTextFile(t, filepath.Join(homeDir, ".codex", "config.toml"))
	if strings.Contains(codexBody, "claude-code-reorient-after-compact") {
		t.Fatal("legacy Codex hook signature was preserved")
	}
	if !strings.Contains(codexBody, "reorient-after-compact") {
		t.Fatal("new Codex after-compact hook signature was missing")
	}
	cursor := readTestCursorHooks(t, filepath.Join(homeDir, ".cursor", "hooks.json"))
	if len(cursor.Hooks[EventCursorStop]) != 1 {
		t.Fatalf("Cursor stop hooks len = %d, want 1", len(cursor.Hooks[EventCursorStop]))
	}
	if cursor.Hooks[EventCursorStop][0].Command != "/usr/local/bin/clyde hooks run reorient-stop-followup" {
		t.Fatalf("Cursor stop command = %q", cursor.Hooks[EventCursorStop][0].Command)
	}
}

func TestInstallerClientCursorWritesOnlyCursorConfig(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installer := Installer{Registry: NewRegistry()}
	result, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientCursor,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(result.Files) != 1 || result.Files[0].Client != ClientCursor {
		t.Fatalf("files = %#v, want only Cursor", result.Files)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".cursor", "hooks.json")); err != nil {
		t.Fatalf("Cursor hooks stat: %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("Claude settings unexpectedly exist, err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".codex", "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("Codex settings unexpectedly exist, err = %v", err)
	}
}

func TestInstallerDryRunWritesNoFiles(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	installer := Installer{Registry: NewRegistry()}
	result, err := installer.Install(context.Background(), InstallOptions{
		HomeDir:  homeDir,
		ClydeBin: "/usr/local/bin/clyde",
		Client:   ClientAll,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.DryRun {
		t.Fatal("DryRun = false, want true")
	}
	if len(result.Files) != 3 {
		t.Fatalf("files len = %d, want 3", len(result.Files))
	}
	for _, file := range result.Files {
		if len(file.Preview) == 0 {
			t.Fatalf("%s preview was empty", file.Client)
		}
		if _, err := os.Stat(file.SettingsPath); !os.IsNotExist(err) {
			t.Fatalf("%s exists after dry run, stat err = %v", file.SettingsPath, err)
		}
	}
}

func assertInstallFile(t *testing.T, result InstallResult, client Client, path string, installed []HookID) {
	t.Helper()

	for _, file := range result.Files {
		if file.Client != client {
			continue
		}
		if file.SettingsPath != path {
			t.Fatalf("%s path = %q, want %q", client, file.SettingsPath, path)
		}
		if !slices.Equal(file.Installed, installed) {
			t.Fatalf("%s installed = %#v, want %#v", client, file.Installed, installed)
		}
		return
	}
	t.Fatalf("missing install file for %s", client)
}

func assertClaudeHandler(t *testing.T, settings testClaudeSettings, event string, matcher string, command string, args []string, timeout int) {
	t.Helper()

	groups := settings.Hooks[event]
	handler := findTestClaudeHookHandler(t, groups, matcher)
	if handler.Type != "command" {
		t.Fatalf("type = %q, want command", handler.Type)
	}
	if handler.Command != command {
		t.Fatalf("%s/%s command = %q, want %q", event, matcher, handler.Command, command)
	}
	if !slices.Equal(handler.Args, args) {
		t.Fatalf("%s/%s args = %#v, want %#v", event, matcher, handler.Args, args)
	}
	if handler.Timeout != timeout {
		t.Fatalf("%s/%s timeout = %d, want %d", event, matcher, handler.Timeout, timeout)
	}
}

func findTestClaudeHookHandler(t *testing.T, groups []testClaudeHookGroup, matcher string) testClaudeHookHandler {
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

func assertCursorCommand(t *testing.T, settings testCursorHooks, event string, command string, loopLimit int) {
	t.Helper()

	for _, hook := range settings.Hooks[event] {
		if hook.Command != command {
			continue
		}
		if hook.LoopLimit != loopLimit {
			t.Fatalf("%s loop_limit = %d, want %d", event, hook.LoopLimit, loopLimit)
		}
		return
	}
	t.Fatalf("missing Cursor command %q under %s", command, event)
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

func readTestCursorHooks(t *testing.T, path string) testCursorHooks {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var settings testCursorHooks
	if err := json.Unmarshal(body, &settings); err != nil {
		t.Fatalf("Unmarshal Cursor hooks: %v\n%s", err, string(body))
	}
	return settings
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(body)
}

func writeTestFile(t *testing.T, path string, body string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
