package localbackendcli_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	sandboxRootBannerPrefix = "root:"
	sandboxStartTimeout     = 30 * time.Second
	sandboxStopTimeout      = 30 * time.Second
	mcpExitSession          = "019e0000-3a00-7010-bd9f-a6ee71559106"
	mcpExitBudget           = 4 * time.Second
)

var (
	clydeBuildDirectory string
	sharedClydeCLI      = sync.OnceValues(compileClydeCLI)
)

func TestMain(m *testing.M) {
	code := m.Run()
	if clydeBuildDirectory != "" {
		if err := os.RemoveAll(clydeBuildDirectory); err != nil {
			fmt.Fprintf(os.Stderr, "The test could not remove the shared clyde build directory %s: %v.\n", clydeBuildDirectory, err)
		}
	}
	os.Exit(code)
}

func buildClydeCLI(t *testing.T) string {
	t.Helper()
	binaryPath, err := sharedClydeCLI()
	if err != nil {
		t.Fatal(err)
	}
	return binaryPath
}

func compileClydeCLI() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(directory, "go.mod")); statErr == nil {
			break
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("go.mod not found above the working directory")
		}
		directory = parent
	}
	clydeBuildDirectory, err = os.MkdirTemp("", "clyde-localbackendcli-")
	if err != nil {
		return "", fmt.Errorf("create clyde CLI build directory: %w", err)
	}
	binaryPath := filepath.Join(clydeBuildDirectory, "clyde")
	command := exec.CommandContext(context.Background(), "go", "build", "-o", binaryPath, "./cmd/clyde")
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build clyde CLI: %w\n%s", err, output)
	}
	return binaryPath, nil
}

// TestMCPServeExitsPromptlyAfterStdinCloses runs one clyde_export_transcript
// call in a new clyde mcp serve process, then closes the process's standard
// input. It checks that the call and process exit finish within mcpExitBudget,
// which is less than the drain limit of five seconds.
func TestMCPServeExitsPromptlyAfterStdinCloses(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	home := t.TempDir()
	item := codexProbeInjectedItem(t, codexProbeResponse)
	installLocalProofTranscript(t, codexProbeRolloutPath(home, mcpExitSession), codexProbeRollout(t, mcpExitSession, item))
	settingsPath := writeSandboxSemanticSettings(t, "")
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--semantic-settings", settingsPath)
	waitForInjectedProbeMCP(t, binaryPath, sandboxDaemon.environment, codexProbeProvider, mcpExitSession)

	export := injectedProbeExport{name: "chat", provider: codexProbeProvider, session: mcpExitSession, only: injectedProbeChatOnly, maxTokens: "", output: "", lastN: injectedProbeLastN}
	started := time.Now()
	injectedProbeMCPBody(t, binaryPath, sandboxDaemon.environment, export)
	if elapsed := time.Since(started); elapsed >= mcpExitBudget {
		t.Errorf("The clyde mcp serve call and process exit took %s, longer than the budget of %s.", elapsed, mcpExitBudget)
	}
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
