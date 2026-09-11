package clispec

import (
	"bytes"
	"strings"
	"testing"
)

func TestDaemonUninstallIsCLIOnlyAndRequiresApply(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	registry := NewConversationRegistry()
	var found bool
	for _, command := range RenderCobra(registry, testFactory(&output)) {
		if command.Name() != "daemon" {
			continue
		}
		for _, child := range command.Commands() {
			if child.Name() != "uninstall" {
				continue
			}
			found = true
			if child.Flags().Lookup("apply") == nil {
				t.Fatal("daemon uninstall must expose --apply")
			}
			if !strings.Contains(child.Short, "unregister") {
				t.Fatalf("uninstall help must describe registration removal: %s", child.Short)
			}
		}
	}
	if !found {
		t.Fatal("daemon uninstall command is missing")
	}
	op := daemonUninstallOp()
	if _, err := op.Prepare(op.New()); err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("uninstall preparation without --apply = %v", err)
	}
	command := op.cobraCommand(testFactory(&output))
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("uninstall without --apply = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("uninstall without --apply wrote output: %s", output.String())
	}
	for _, operation := range registry.ops {
		if operation.surfaceSet().MCP && operation.group() == daemonGroup {
			if operation.cobraCommand(testFactory(&output)).Name() == "uninstall" {
				t.Fatal("daemon uninstall must not be available through MCP")
			}
		}
	}
}
