package localbackendcli_test

import (
	"context"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemon/localtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// waitForFirstMatches retries until the daemon accepts the search and the search
// returns want matches. An error other than Unavailable fails the test at once.
func waitForFirstMatches(t *testing.T, options conversation.SearchConversationsOptions, want int) {
	t.Helper()
	deadline := time.Now().Add(localtest.RawTextSearchTimeout)
	for time.Now().Before(deadline) {
		result, err := daemon.SearchConversations(context.Background(), options)
		if err != nil && status.Code(err) != codes.Unavailable {
			t.Fatalf("SearchConversations: %v", err)
		}
		if err == nil && len(result.Matches) == want {
			return
		}
		time.Sleep(localtest.RawTextPollInterval)
	}
	t.Fatalf("SearchConversations did not return %d matches within %s", want, localtest.RawTextSearchTimeout)
}
