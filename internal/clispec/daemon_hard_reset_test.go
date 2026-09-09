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
			}
		}
	}
	if !found {
		t.Fatal("daemon hard-reset command is missing")
	}
	for _, operation := range registry.ops {
		command := operation.cobraCommand(testFactory(&output))
		if command.Name() == "hard-reset" && operation.surfaceSet().MCP {
			t.Fatal("hard reset must not be available through MCP")
		}
	}
}
