package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const embeddingRefusalCause = "context_length_exceeded at unix:///private/run/search.sock from lm-semantic-search using NV-EmbedCode-7b-v1 with credential sk-private"

func conversationSearchSocketPath(t *testing.T) string {
	t.Helper()
	socketFile, err := os.CreateTemp("/tmp", "clyde-search-refusal-*.sock")
	if err != nil {
		t.Fatalf("create daemon socket path: %v", err)
	}
	socketPath := socketFile.Name()
	if err := socketFile.Close(); err != nil {
		t.Fatalf("close daemon socket placeholder: %v", err)
	}
	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove daemon socket placeholder: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove daemon socket: %v", err)
		}
	})
	return socketPath
}

func buildConversationSearchCLI(t *testing.T) string {
	t.Helper()
	repositoryRoot, err := daemonTestRepositoryRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	binaryPath := filepath.Join(t.TempDir(), "clyde")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binaryPath, "./cmd/clyde")
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build clyde CLI: %v\n%s", err, output)
	}
	return binaryPath
}

func daemonTestRepositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", os.ErrNotExist
		}
		directory = parent
	}
}

func environmentWithOverrides(overrides ...string) []string {
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
