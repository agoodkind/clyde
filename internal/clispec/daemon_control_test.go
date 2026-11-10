package clispec

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

func TestDaemonStopAndDisableRequireApplyAndPrintHelp(t *testing.T) {
	for _, name := range []string{"stop", "disable"} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			root := &cobra.Command{Use: "clyde", SilenceErrors: true}
			root.SetOut(&output)
			root.SetErr(&output)
			root.AddCommand(RenderCobra(NewConversationRegistry(), testFactory(&output))...)
			cli.InstallHelpRendering(root)
			root.SetArgs([]string{"daemon", name})

			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "--apply") {
				t.Fatalf("daemon %s without --apply = %v", name, err)
			}
			rendered := output.String()
			if !strings.Contains(rendered, "Usage:") || !strings.Contains(rendered, "--apply") {
				t.Fatalf("daemon %s did not print apply-gated help:\n%s", name, rendered)
			}
		})
	}
}

func TestDaemonStopAndDisableAreCLIOnly(t *testing.T) {
	t.Parallel()
	registry := NewConversationRegistry()
	want := map[string]bool{"stop": false, "disable": false}
	for _, operation := range registry.ops {
		command := operation.cobraCommand(testFactory(&bytes.Buffer{}))
		if _, ok := want[command.Name()]; !ok {
			continue
		}
		want[command.Name()] = true
		if operation.surfaceSet().MCP {
			t.Fatalf("daemon %s must not be available through MCP", command.Name())
		}
		if command.Flags().Lookup("apply") == nil {
			t.Fatalf("daemon %s must expose --apply", command.Name())
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("daemon %s command is missing", name)
		}
	}
}
