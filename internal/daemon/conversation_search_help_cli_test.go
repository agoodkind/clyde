package daemon_test

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/sandbox"
)

const (
	sandboxRootBannerPrefix   = "root:"
	sandboxStartTimeout       = 30 * time.Second
	sandboxBrowseReadyTimeout = 60 * time.Second
	sandboxBrowseRetryDelay   = 250 * time.Millisecond
	sandboxStopTimeout        = 30 * time.Second
)

func TestConversationSearchCommandWithoutInputPrintsHelp(t *testing.T) {
	binaryPath := buildClydeCLI(t)
	fakeHome := t.TempDir()

	helpCommand := exec.CommandContext(t.Context(), binaryPath, "conversation", "search")
	helpCommand.Env = environmentWith(
		"HOME="+fakeHome,
		"XDG_CACHE_HOME="+t.TempDir(),
		"XDG_CONFIG_HOME="+t.TempDir(),
		"XDG_RUNTIME_DIR="+t.TempDir(),
		"XDG_STATE_HOME="+t.TempDir(),
	)
	helpBytes, err := helpCommand.CombinedOutput()
	helpOutput := string(helpBytes)
	if err != nil {
		t.Fatalf("conversation search without input and without a daemon: %v\n%s", err, helpOutput)
	}
	for _, expected := range []string{"Usage:", "clyde conversation search [CONVERSATION_ID]", "--query"} {
		if !strings.Contains(helpOutput, expected) {
			t.Fatalf("conversation search help missing %q:\n%s", expected, helpOutput)
		}
	}
	if strings.Contains(helpOutput, "total_matched:") {
		t.Fatalf("conversation search without input listed conversations:\n%s", helpOutput)
	}

	roots := startSandboxDaemon(t, binaryPath, fakeHome)
	sandboxEnvironment := []string{"HOME=" + fakeHome}
	for _, variable := range sandbox.Env(roots) {
		sandboxEnvironment = append(sandboxEnvironment, variable.Name+"="+variable.Value)
	}

	deadline := time.Now().Add(sandboxBrowseReadyTimeout)
	var browseOutput string
	for {
		browseCommand := exec.CommandContext(t.Context(), binaryPath, "conversation", "search", "--limit", "20")
		browseCommand.Env = environmentWith(sandboxEnvironment...)
		browseBytes, browseErr := browseCommand.CombinedOutput()
		browseOutput = string(browseBytes)
		if browseErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation search --limit 20 against the sandbox daemon: %v\n%s", browseErr, browseOutput)
		}
		time.Sleep(sandboxBrowseRetryDelay)
	}
	for _, expected := range []string{"total_matched:", "limit: 20"} {
		if !strings.Contains(browseOutput, expected) {
			t.Fatalf("conversation search --limit 20 output missing %q:\n%s", expected, browseOutput)
		}
	}
	if strings.Contains(browseOutput, "Usage:") {
		t.Fatalf("conversation search --limit 20 printed help:\n%s", browseOutput)
	}
}

func startSandboxDaemon(t *testing.T, binaryPath string, home string) sandbox.Roots {
	t.Helper()
	command := exec.Command(binaryPath, "daemon", "sandbox")
	command.Env = environmentWith("HOME=" + home)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("open sandbox daemon stdout: %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start sandbox daemon: %v", err)
	}
	exited := make(chan error, 1)
	rootLine := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if root, found := strings.CutPrefix(line, sandboxRootBannerPrefix); found {
				select {
				case rootLine <- strings.TrimSpace(root):
				default:
				}
			}
		}
		exited <- command.Wait()
	}()
	t.Cleanup(func() {
		if err := command.Process.Signal(os.Interrupt); err != nil {
			t.Logf("interrupt sandbox daemon: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(sandboxStopTimeout):
			_ = command.Process.Kill()
			t.Errorf("sandbox daemon did not stop after interrupt; stderr:\n%s", stderr.String())
		}
	})

	select {
	case base := <-rootLine:
		return sandbox.Roots{
			Base:    base,
			State:   filepath.Join(base, "state"),
			Config:  filepath.Join(base, "config"),
			Cache:   filepath.Join(base, "cache"),
			Runtime: filepath.Join(base, "run"),
		}
	case exitErr := <-exited:
		exited <- exitErr
		t.Fatalf("sandbox daemon exited before printing its root: %v\n%s", exitErr, stderr.String())
	case <-time.After(sandboxStartTimeout):
		t.Fatalf("sandbox daemon did not print its root; stderr:\n%s", stderr.String())
	}
	return sandbox.Roots{Base: "", State: "", Config: "", Cache: "", Runtime: ""}
}

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
