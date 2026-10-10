package localbackend_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clidaemon "goodkind.io/clyde/internal/cli/daemon"
	"goodkind.io/clyde/internal/clispec"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemon/localtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	localBackendSearchTimeout = 3 * time.Minute
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

func TestLocalBackendIngestsAndSearchesWithoutMilvus(t *testing.T) {
	localtest.StartRawTextDaemon(t, localBackendDaemonConfig)
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
		time.Sleep(localtest.RawTextPollInterval)
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
	cliOutput := localtest.LocalBackendSearchCLIOutput(t, registry, options.Query)
	if !strings.Contains(cliOutput, localtest.LocalBackendSourceJSON) {
		t.Fatalf("conversation search output missing %s:\n%s", localtest.LocalBackendSourceJSON, cliOutput)
	}
	mcpOutput := localtest.LocalBackendSearchMCPOutput(t, registry, options.Query)
	if !strings.Contains(mcpOutput, localtest.LocalBackendSourceJSON) {
		t.Fatalf("clyde_search output missing %s:\n%s", localtest.LocalBackendSourceJSON, mcpOutput)
	}
	if !strings.Contains(strings.ToLower(result.Matches[0].Snippet), "rebind") {
		t.Fatalf("top match snippet = %q, want a fixture message about the listener rebind", result.Matches[0].Snippet)
	}
	localRoot := filepath.Join(os.Getenv("XDG_STATE_HOME"), "clyde", "conversation-local")
	if _, err := os.Stat(localRoot); err != nil {
		t.Fatalf("local store directory %s: %v", localRoot, err)
	}
}
