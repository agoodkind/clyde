package localbackend_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	sandboxRootBannerPrefix = "root:"
	sandboxStartTimeout     = 30 * time.Second
	sandboxStopTimeout      = 30 * time.Second
)

func buildClydeCLI(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(directory, "go.mod")); statErr == nil {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("go.mod not found above the working directory")
		}
		directory = parent
	}
	binaryPath := filepath.Join(t.TempDir(), "clyde")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binaryPath, "./cmd/clyde")
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build clyde CLI: %v\n%s", err, output)
	}
	return binaryPath
}

func environmentWith(overrides ...string) []string {
	overrideKeys := make(map[string]bool, len(overrides))
	for _, override := range overrides {
		key, _, found := strings.Cut(override, "=")
		if found {
			overrideKeys[key] = true
		}
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && overrideKeys[key] {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, overrides...)
}
