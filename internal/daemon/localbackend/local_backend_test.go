package localbackend_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/spf13/cobra"
	"goodkind.io/clyde/internal/cli"
	clidaemon "goodkind.io/clyde/internal/cli/daemon"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	localBackendSearchTimeout = 3 * time.Minute
	localBackendSourceJSON    = `"source":"local"`
	// A missing key file must not prevent the local backend from starting.
	localBackendDaemonConfig = `[conversation.semantic]
ingestion_enabled = true
search_enabled = true
backend = "local"
embedding_api_key_file = "/nonexistent/clyde-local-backend-test-key"

[adapter]
enabled = false

[mitm]
enabled_default = false
`
)

func localBackendSearchCLIOutput(t *testing.T, registry *clispec.Registry, query string) string {
	t.Helper()
	var stdout bytes.Buffer
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{In: &bytes.Buffer{}, Out: &stdout, Err: &bytes.Buffer{}},
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

func localBackendSearchMCPOutput(t *testing.T, registry *clispec.Registry, query string) string {
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

func TestLocalBackendIngestsAndSearchesWithoutMilvus(t *testing.T) {
	startRawTextDaemon(t, localBackendDaemonConfig)
	options := conversation.SearchConversationsOptions{
		Query: "why does the daemon bind its listener again after a configuration edit",
		Limit: 5,
	}

	var result conversation.SearchConversationsResult
	deadline := time.Now().Add(localBackendSearchTimeout)
	for time.Now().Before(deadline) {
		found, err := daemon.SearchConversations(context.Background(), options)
		if err != nil && status.Code(err) != codes.Unavailable {
			t.Fatalf("SearchConversations: %v", err)
		}
		if err == nil && found.Source != conversation.SearchSourceRawText && len(found.Matches) > 0 {
			result = found
			break
		}
		time.Sleep(rawTextPollInterval)
	}
	if len(result.Matches) == 0 {
		t.Fatalf("SearchConversations did not return a local backend match within %s", localBackendSearchTimeout)
	}
	if result.Source != conversation.SearchSourceLocal {
		t.Fatalf("search source = %s, want %s", result.Source, conversation.SearchSourceLocal)
	}
	report := daemon.InspectStatus(context.Background())
	if report.Runtime == nil {
		t.Fatalf("daemon status has no runtime snapshot: %s", report.DaemonError)
	}
	if report.Runtime.Semantic.Backend != "local" {
		t.Fatalf("semantic status backend = %q, want %q", report.Runtime.Semantic.Backend, "local")
	}
	var statusText bytes.Buffer
	clidaemon.WriteRuntimeStatusReport(&statusText, report.Runtime)
	if !strings.Contains(statusText.String(), "backend=local") {
		t.Fatalf("status text missing backend=local:\n%s", statusText.String())
	}
	registry := clispec.NewConversationRegistry()
	cliOutput := localBackendSearchCLIOutput(t, registry, options.Query)
	if !strings.Contains(cliOutput, localBackendSourceJSON) {
		t.Fatalf("conversation search output missing %s:\n%s", localBackendSourceJSON, cliOutput)
	}
	mcpOutput := localBackendSearchMCPOutput(t, registry, options.Query)
	if !strings.Contains(mcpOutput, localBackendSourceJSON) {
		t.Fatalf("clyde_search output missing %s:\n%s", localBackendSourceJSON, mcpOutput)
	}
	if !strings.Contains(strings.ToLower(result.Matches[0].Snippet), "rebind") {
		t.Fatalf("top match snippet = %q, want a fixture message about the listener rebind", result.Matches[0].Snippet)
	}
	localRoot := filepath.Join(os.Getenv("XDG_STATE_HOME"), "clyde", "conversation-local")
	if _, err := os.Stat(localRoot); err != nil {
		t.Fatalf("local store directory %s: %v", localRoot, err)
	}
}
