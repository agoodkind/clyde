package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	localBackendSearchTimeout = 3 * time.Minute
	// The embedding API key file does not exist. The local backend calls no
	// embedding endpoint and must start without it.
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
		if err == nil && found.Source == conversation.SearchSourceSemantic && len(found.Matches) > 0 {
			result = found
			break
		}
		time.Sleep(rawTextPollInterval)
	}
	if len(result.Matches) == 0 {
		t.Fatalf("SearchConversations did not return a semantic match within %s", localBackendSearchTimeout)
	}
	// Both fixture messages are about the listener rebind.
	if !strings.Contains(strings.ToLower(result.Matches[0].Snippet), "rebind") {
		t.Fatalf("top match snippet = %q, want a fixture message about the listener rebind", result.Matches[0].Snippet)
	}
	localRoot := filepath.Join(os.Getenv("XDG_STATE_HOME"), "clyde", "conversation-local")
	if _, err := os.Stat(localRoot); err != nil {
		t.Fatalf("local store directory %s: %v", localRoot, err)
	}
}
