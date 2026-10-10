// Package localtest provides helpers shared by localbackend and localbackendcli tests.
package localtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/sandbox"
)

const (
	// RawTextReadableSession identifies the first transcript StartRawTextDaemon writes.
	RawTextReadableSession = "raw-text-readable-session"
	// RawTextUnreadableSession identifies the second transcript StartRawTextDaemon writes.
	RawTextUnreadableSession = "raw-text-unreadable-session"
	// RawTextSearchTimeout limits waits for search results and daemon shutdown during test cleanup.
	RawTextSearchTimeout = 30 * time.Second
	// RawTextPollInterval sets the delay between search attempts while tests await results.
	RawTextPollInterval = 200 * time.Millisecond
	// LocalBackendSourceJSON identifies local backend matches in CLI and MCP search output.
	LocalBackendSourceJSON = `"source":"local"`
)

// WriteRawTextTranscript sets both timestamps to the specified minute.
func WriteRawTextTranscript(t *testing.T, path string, session string, minute int) {
	t.Helper()
	record := func(index int, role string, body string) string {
		head := fmt.Sprintf(`"sessionId":%q,"cwd":"/repo","uuid":"%02d%06d-0000-4000-8000-000000000000","timestamp":"2026-07-01T12:%02d:%02dZ"`, session, minute, index, minute, index)
		if role == "user" {
			return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},%s}`, body, head)
		}
		return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`, body, head)
	}
	lines := []string{
		record(0, "user", "why does the listener rebind on reload"),
		record(1, "assistant", "The Watcher classifies the CONFIG change before the daemon rebinds."),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	slog.Debug("localtest.transcript.written", "path", path, "session", session)
}

// StartRawTextDaemon runs the daemon in this process with every Clyde path and
// every provider store under temporary directories. It returns the Claude
// project directory.
func StartRawTextDaemon(t *testing.T, daemonConfig string) string {
	t.Helper()
	roots, err := sandbox.NewRoots()
	if err != nil {
		t.Fatalf("create sandbox roots: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(roots.Base) })
	for _, variable := range sandbox.Env(roots) {
		t.Setenv(variable.Name, variable.Value)
	}
	home := filepath.Join(roots.Base, "home")
	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	WriteRawTextTranscript(t, filepath.Join(projectDir, RawTextReadableSession+".jsonl"), RawTextReadableSession, 0)
	WriteRawTextTranscript(t, filepath.Join(projectDir, RawTextUnreadableSession+".jsonl"), RawTextUnreadableSession, 1)
	t.Setenv("HOME", home)
	for _, name := range []string{"CODEX_HOME", "CODEX_SQLITE_HOME", "CLYDE_CURSOR_PROJECTS_DIRS", "CLYDE_CURSOR_DATA_DIRS", "CLYDE_ZED_DATA_DIRS", "COPILOT_HOME"} {
		directory := filepath.Join(roots.Base, "providers", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
		t.Setenv(name, directory)
	}
	configPath := filepath.Join(roots.Config, "clyde", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte(daemonConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicErr := fmt.Errorf("daemon run panicked: %v", recovered)
				slog.Error("localtest.daemon.panicked", "err", panicErr)
				done <- panicErr
			}
		}()
		done <- daemon.RunContext(ctx, slog.Default())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-done:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("daemon run: %v", runErr)
			}
		case <-time.After(RawTextSearchTimeout):
			t.Error("daemon did not stop")
		}
	})
	return projectDir
}

// LocalBackendSearchCLIOutput searches the daemon in the test process through Cobra commands rendered from registry.
func LocalBackendSearchCLIOutput(t *testing.T, registry *clispec.Registry, query string) string {
	t.Helper()
	var stdout bytes.Buffer
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{In: &bytes.Buffer{}, Out: &stdout, Err: &bytes.Buffer{}},
		Logger:    nil,
		Build:     cli.BuildInfo{Version: "", Commit: "", Date: ""},
		Verbose:   nil,
		Copy:      nil,
		Config:    nil,
	}
	root := &cobra.Command{Use: "clyde"}
	output.PersistentFlag(root)
	root.AddCommand(clispec.RenderCobra(registry, factory)...)
	root.SetArgs([]string{"--output-format", "json", "conversation", "search", "--query", query})
	if err := root.Execute(); err != nil {
		t.Fatalf("conversation search: %v\n%s", err, stdout.String())
	}
	return stdout.String()
}

// LocalBackendSearchMCPOutput returns the JSON-RPC response from the clyde_search tool rendered from registry as JSON text.
func LocalBackendSearchMCPOutput(t *testing.T, registry *clispec.Registry, query string) string {
	t.Helper()
	mcpServer := server.NewMCPServer("clyde-local-backend-test", "test")
	clispec.RenderMCP(registry, mcpServer)
	request := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"clyde_search","arguments":{"query":%q}}}`,
		query,
	)
	response := mcpServer.HandleMessage(context.Background(), json.RawMessage(request))
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("encode clyde_search response: %v", err)
	}
	return string(body)
}
