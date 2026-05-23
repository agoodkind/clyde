package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogsInventoryCommandDetectedThroughFlags(t *testing.T) {
	args := []string{"--config", "/tmp/clyde.toml", "logs", "inventory", "--state-root", "/tmp/state"}

	if !isReadOnlyLogsInventoryCommand(args) {
		t.Fatalf("logs inventory command was not detected")
	}
}

func TestReadOnlyLogsInventorySkipsLoggingSetupStateWrites(t *testing.T) {
	stateHome := t.TempDir()
	configHome := t.TempDir()
	inventoryRoot := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	logPath := filepath.Join(inventoryRoot, "clyde-daemon.jsonl")
	if err := os.WriteFile(logPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write inventory fixture: %v", err)
	}

	exitCode := runReadOnlyLogsCommand([]string{"logs", "inventory", "--state-root", inventoryRoot})
	if exitCode != 0 {
		t.Fatalf("exitCode=%d, want 0", exitCode)
	}

	defaultStateRoot := filepath.Join(stateHome, "clyde")
	if _, err := os.Stat(defaultStateRoot); !os.IsNotExist(err) {
		t.Fatalf("read-only inventory created default state root %s: %v", defaultStateRoot, err)
	}
}
