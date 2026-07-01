package clispec

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli/output"
)

func TestInstallHooksCommandWritesUserHookSettings(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	var out bytes.Buffer
	root := &cobra.Command{Use: "clyde"}
	output.PersistentFlag(root)
	for _, command := range RenderCobra(NewConversationRegistry(), testFactory(&out)) {
		root.AddCommand(command)
	}
	root.SetArgs([]string{
		"install",
		"hooks",
		"--clyde-bin",
		"/usr/local/bin/clyde",
	})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute install hooks: %v", err)
	}

	claudePath := filepath.Join(homeDir, ".claude", "settings.json")
	claudeBody, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("ReadFile Claude settings: %v", err)
	}
	if bytes.Contains(claudeBody, []byte("claude-code-reorient-after-compact")) {
		t.Fatalf("Claude settings retained legacy hook id:\n%s", string(claudeBody))
	}
	if !bytes.Contains(claudeBody, []byte(`"command": "/usr/local/bin/clyde"`)) ||
		!bytes.Contains(claudeBody, []byte(`"reorient-after-compact"`)) ||
		!bytes.Contains(claudeBody, []byte(`"reorient-before-compact"`)) {
		t.Fatalf("Claude settings missing reorient hooks:\n%s", string(claudeBody))
	}
	codexBody, err := os.ReadFile(filepath.Join(homeDir, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("ReadFile Codex settings: %v", err)
	}
	if !bytes.Contains(codexBody, []byte("[[hooks.session_start]]")) ||
		!bytes.Contains(codexBody, []byte("[[hooks.pre_compact]]")) ||
		!bytes.Contains(codexBody, []byte(`command = "/usr/local/bin/clyde hooks run reorient-after-compact"`)) ||
		!bytes.Contains(codexBody, []byte(`command = "/usr/local/bin/clyde hooks run reorient-before-compact"`)) {
		t.Fatalf("Codex settings missing expected hooks:\n%s", string(codexBody))
	}
	cursorBody, err := os.ReadFile(filepath.Join(homeDir, ".cursor", "hooks.json"))
	if err != nil {
		t.Fatalf("ReadFile Cursor hooks: %v", err)
	}
	if !bytes.Contains(cursorBody, []byte(`/usr/local/bin/clyde hooks run reorient-stop-followup`)) {
		t.Fatalf("Cursor hooks missing stop followup hook:\n%s", string(cursorBody))
	}
	if !bytes.Contains(out.Bytes(), []byte("installed hooks")) {
		t.Fatalf("output missing install summary:\n%s", out.String())
	}
}
