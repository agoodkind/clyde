package hookspec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallRemovesOnlyClydeHooks(t *testing.T) {
	t.Parallel()
	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, ".claude", "settings.json"), `{"theme":"dark","hooks":{"PreCompact":[{"hooks":[{"type":"command","command":"/bin/echo unrelated"}]}]}}`)
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), "approval_policy = \"never\"\n")
	writeTestFile(t, filepath.Join(homeDir, ".cursor", "hooks.json"), `{"version":1,"extra":true,"hooks":{"preToolUse":[{"command":"/bin/echo unrelated"}]}}`)
	installer := Installer{Registry: NewRegistry()}
	if _, err := installer.Install(context.Background(), InstallOptions{HomeDir: homeDir, ClydeBin: "/usr/local/bin/clyde"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	result, err := (Uninstaller{Registry: NewRegistry()}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !result.Changed {
		t.Fatal("Changed = false, want true")
	}
	for _, path := range []string{
		filepath.Join(homeDir, ".claude", "settings.json"),
		filepath.Join(homeDir, ".codex", "config.toml"),
		filepath.Join(homeDir, ".cursor", "hooks.json"),
	} {
		body := readTextFile(t, path)
		if strings.Contains(body, "clyde hooks run") {
			t.Fatalf("Clyde hook remains in %s:\n%s", path, body)
		}
	}
	if body := readTextFile(t, filepath.Join(homeDir, ".claude", "settings.json")); !strings.Contains(body, "dark") || !strings.Contains(body, "/bin/echo unrelated") {
		t.Fatalf("Claude unrelated settings changed:\n%s", body)
	}
	if body := readTextFile(t, filepath.Join(homeDir, ".codex", "config.toml")); !strings.Contains(body, "approval_policy = \"never\"") {
		t.Fatalf("Codex unrelated settings changed:\n%s", body)
	}
	if body := readTextFile(t, filepath.Join(homeDir, ".cursor", "hooks.json")); !strings.Contains(body, "\"extra\": true") || !strings.Contains(body, "/bin/echo unrelated") {
		t.Fatalf("Cursor unrelated settings changed:\n%s", body)
	}
}

func TestUninstallPreservesForeignClydeExecutables(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	writeTestFile(t, filepath.Join(homeDir, ".claude", "settings.json"), `{"hooks":{"PreCompact":[{"hooks":[{"type":"command","command":"/opt/other/clyde","args":["hooks","run","reorient","before-compact"]}]}]}}`)
	writeTestFile(t, filepath.Join(homeDir, ".codex", "config.toml"), "[[hooks.pre_compact]]\n\n[[hooks.pre_compact.hooks]]\ncommand = \"/opt/other/clyde hooks run reorient-before-compact\"\n")
	writeTestFile(t, filepath.Join(homeDir, ".cursor", "hooks.json"), `{"hooks":{"preToolUse":[{"command":"/opt/other/clyde hooks run reorient before-compact"}]}}`)

	result, err := (Uninstaller{Registry: NewRegistry()}).Uninstall(context.Background(), UninstallOptions{HomeDir: homeDir})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if result.Changed {
		t.Fatal("Changed = true, want false")
	}
	for _, path := range []string{
		filepath.Join(homeDir, ".claude", "settings.json"),
		filepath.Join(homeDir, ".codex", "config.toml"),
		filepath.Join(homeDir, ".cursor", "hooks.json"),
	} {
		body := readTextFile(t, path)
		if !strings.Contains(body, "/opt/other/clyde") {
			t.Fatalf("foreign Clyde executable was removed from %s:\n%s", path, body)
		}
	}
}
