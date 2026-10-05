package daemon_test

import (
	"context"
	"fmt"
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
	localBackendDaemonConfig  = `[conversation.semantic]
ingestion_enabled = true
search_enabled = true
backend = "local"
embedding_model = "bge-small"
model_cache_root = %q

[adapter]
enabled = false

[mitm]
enabled_default = false
`
)

// The lm-semantic-search ONNX provider tests download the pinned model files
// into this cache once per machine.
func localBackendModelCache(t *testing.T) string {
	t.Helper()
	root := filepath.Join(os.TempDir(), "lm-semantic-search-offline-model-test-cache")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create model cache %s: %v", root, err)
	}
	return root
}

func TestLocalBackendIngestsAndSearchesWithoutMilvus(t *testing.T) {
	startRawTextDaemon(t, fmt.Sprintf(localBackendDaemonConfig, localBackendModelCache(t)))
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
