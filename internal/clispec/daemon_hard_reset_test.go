package clispec

import (
	"bytes"
	"strings"
	"testing"
)

func TestDaemonHardResetIsCLIOnly(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	registry := NewConversationRegistry()
	found := false
	for _, command := range RenderCobra(registry, testFactory(&output)) {
		if command.Name() != "daemon" {
			continue
		}
		for _, child := range command.Commands() {
			if child.Name() == "hard-reset" {
				found = true
				if !strings.Contains(child.Short, "delete") {
					t.Fatalf("reset help must describe deletion: %s", child.Short)
				}
				if !strings.Contains(child.Long, "Use config only") {
					t.Fatalf("reset help must reserve config deletion for explicit scope: %s", child.Long)
				}
			}
		}
	}
	if !found {
		t.Fatal("daemon hard-reset command is missing")
	}
	op := daemonHardResetOp()
	if _, err := op.Prepare(op.New()); err != nil {
		t.Fatal(err)
	}
	command := op.cobraCommand(testFactory(&output))
	if command.Flags().Lookup("apply") == nil {
		t.Fatal("hard reset must require an apply flag")
	}
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("hard reset without --apply = %v", err)
	}
	for _, operation := range registry.ops {
		command := operation.cobraCommand(testFactory(&output))
		if command.Name() == "hard-reset" && operation.surfaceSet().MCP {
			t.Fatal("hard reset must not be available through MCP")
		}
	}
}
