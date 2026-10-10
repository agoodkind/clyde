package statusproof_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func ingestionProofSearch(t *testing.T, query string) (conversation.SearchConversationsResult, bool) {
	t.Helper()
	result, err := daemon.SearchConversations(context.Background(), conversation.SearchConversationsOptions{
		Query: query,
		Limit: ingestionProofSearchLimit,
	})
	if err != nil {
		if status.Code(err) == codes.Unavailable {
			return result, false
		}
		t.Fatalf("operation=search_conversations query=%q err=%v", query, err)
	}
	return result, result.Source == conversation.SearchSourceLocal
}

func ingestionProofHasMatch(result conversation.SearchConversationsResult, session string, token string) bool {
	for _, match := range result.Matches {
		if match.Record.NativeID != session {
			continue
		}
		if strings.Contains(match.Snippet, token) || strings.Contains(match.ContextWindow, token) {
			return true
		}
	}
	return false
}

func ingestionProofDescribe(result conversation.SearchConversationsResult) string {
	described := make([]string, 0, len(result.Matches))
	for _, match := range result.Matches {
		described = append(described, fmt.Sprintf("%s#%d:%q", match.Record.NativeID, match.MessageIndex, match.Snippet))
	}
	return strings.Join(described, " | ")
}

func waitForIngestionProofMatch(t *testing.T, step string, query string, session string, token string) {
	t.Helper()
	var last conversation.SearchConversationsResult
	deadline := time.Now().Add(ingestionProofPassTimeout)
	for time.Now().Before(deadline) {
		result, answered := ingestionProofSearch(t, query)
		if answered {
			last = result
			if ingestionProofHasMatch(result, session, token) {
				return
			}
		}
		time.Sleep(ingestionProofPollInterval)
	}
	t.Fatalf("step=%s operation=wait_for_match session=%s token=%s timeout=%s matches=[%s]",
		step, session, token, ingestionProofPassTimeout, ingestionProofDescribe(last))
}

func assertIngestionProofMatch(t *testing.T, step string, query string, session string, token string, want bool) {
	t.Helper()
	result, answered := ingestionProofSearch(t, query)
	if !answered {
		t.Errorf("step=%s operation=search_conversations query=%q source=%s", step, query, result.Source)
		return
	}
	if found := ingestionProofHasMatch(result, session, token); found != want {
		t.Errorf("step=%s session=%s token=%s found=%t want=%t matches=[%s]",
			step, session, token, found, want, ingestionProofDescribe(result))
	}
}

func waitForIngestionProofFreshness(
	t *testing.T,
	step string,
	timeout time.Duration,
	accept func(conversation.SearchFreshness) bool,
) conversation.SearchFreshness {
	t.Helper()
	var last conversation.SearchFreshness
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		freshness, err := daemon.GetSearchFreshness(context.Background())
		if err != nil {
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("step=%s operation=get_search_freshness err=%v", step, err)
			}
		} else {
			last = freshness
			if accept(freshness) {
				return freshness
			}
		}
		time.Sleep(ingestionProofPollInterval)
	}
	t.Fatalf("step=%s operation=wait_for_freshness timeout=%s freshness=%+v", step, timeout, last)
	return last
}

func waitForIngestionProofListing(t *testing.T, step string, session string) {
	t.Helper()
	deadline := time.Now().Add(ingestionProofPassTimeout)
	for time.Now().Before(deadline) {
		listed, err := daemon.ListConversations(context.Background(), conversation.ListOptions{Limit: ingestionProofListLimit})
		if err != nil {
			t.Fatalf("step=%s operation=list_conversations err=%v", step, err)
		}
		for _, record := range listed.Records {
			if record.NativeID == session {
				return
			}
		}
		time.Sleep(ingestionProofPollInterval)
	}
	t.Fatalf("step=%s operation=wait_for_listing session=%s timeout=%s", step, session, ingestionProofPassTimeout)
}
