package clispec

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli/output"
)

func TestInstallHooksCommandWritesUserClaudeSettings(t *testing.T) {
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

	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("ReadFile settings: %v", err)
	}
	if !bytes.Contains(body, []byte("claude-code-reorient-after-compact")) {
		t.Fatalf("settings missing reorient hook:\n%s", string(body))
	}
	if !bytes.Contains(out.Bytes(), []byte("installed hooks")) {
		t.Fatalf("output missing install summary:\n%s", out.String())
	}
}
