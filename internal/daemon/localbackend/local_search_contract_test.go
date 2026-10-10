package localbackend_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemon/localtest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	localSearchContractQuery       = "listener rebind after a configuration reload"
	localSearchArchivedSession     = "019e0000-3a00-7010-bd9f-a6ee71559357"
	localSearchArchivedRows        = 17000
	localSearchArchivedProbeOffset = 16300
	localSearchArchivedProbeLimit  = 50
	localSearchExtraSessions       = 5
	localSearchMessagesPerSession  = 2
	localSearchFirstExtraMinute    = 2
	localSearchReadyTimeout        = 5 * time.Minute
	localSearchMissingWorkspace    = "/local-search-contract/no-such-workspace"
)

type localSearchMatchKey struct {
	conversationID string
	messageIndex   int
	role           string
	score          float64
}

func localSearchMatchKeys(matches []conversation.SearchMatch) []localSearchMatchKey {
	keys := make([]localSearchMatchKey, 0, len(matches))
	for _, match := range matches {
		keys = append(keys, localSearchMatchKey{
			conversationID: match.Record.ID,
			messageIndex:   match.MessageIndex,
			role:           match.Role,
			score:          match.Score,
		})
	}
	return keys
}

func writeLocalSearchArchivedRollout(t *testing.T) {
	t.Helper()
	archivedDir := filepath.Join(os.Getenv("CODEX_HOME"), "archived_sessions")
	if err := os.MkdirAll(archivedDir, 0o700); err != nil {
		t.Fatalf("create archived sessions dir: %v", err)
	}
	var body bytes.Buffer
	fmt.Fprintf(&body, `{"timestamp":"2026-07-01T11:00:00.000Z","type":"session_meta","payload":{"id":%q,"timestamp":"2026-07-01T11:00:00.000Z","cwd":"/archived","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`+"\n", localSearchArchivedSession)
	for range localSearchArchivedRows {
		fmt.Fprintf(&body, `{"timestamp":"2026-07-01T11:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":%q}}`+"\n", localSearchContractQuery)
	}
	path := filepath.Join(archivedDir, "rollout-2026-07-01T11-00-00-"+localSearchArchivedSession+".jsonl")
	if err := os.WriteFile(path, body.Bytes(), 0o600); err != nil {
		t.Fatalf("write archived rollout: %v", err)
	}
}

func waitForLocalSearch(
	t *testing.T,
	deadline time.Time,
	options conversation.SearchConversationsOptions,
	ready func(conversation.SearchConversationsResult) bool,
	want string,
) {
	t.Helper()
	returned := -1
	for time.Now().Before(deadline) {
		result, err := daemon.SearchConversations(context.Background(), options)
		if err != nil && status.Code(err) != codes.Unavailable {
			t.Fatalf("SearchConversations: %v", err)
		}
		if err == nil {
			returned = len(result.Matches)
			if ready(result) {
				return
			}
		}
		time.Sleep(localtest.RawTextPollInterval)
	}
	t.Fatalf("the local index did not hold %s within %s; the last search returned %d matches", want, localSearchReadyTimeout, returned)
}

func searchLocalContract(t *testing.T, options conversation.SearchConversationsOptions) conversation.SearchConversationsResult {
	t.Helper()
	result, err := daemon.SearchConversations(context.Background(), options)
	if err != nil {
		t.Fatalf("SearchConversations(%+v): %v", options, err)
	}
	return result
}

func TestLocalSearchRanksEveryAllowedPassage(t *testing.T) {
	projectDir := localtest.StartRawTextDaemon(t, localBackendDaemonConfig)
	sessions := []string{localtest.RawTextReadableSession, localtest.RawTextUnreadableSession}
	for extra := range localSearchExtraSessions {
		session := fmt.Sprintf("local-search-visible-session-%d", extra)
		localtest.WriteRawTextTranscript(t, filepath.Join(projectDir, session+".jsonl"), session, extra+localSearchFirstExtraMinute)
		sessions = append(sessions, session)
	}
	writeLocalSearchArchivedRollout(t)

	deadline := time.Now().Add(localSearchReadyTimeout)
	for _, session := range sessions {
		scoped := conversation.SearchConversationsOptions{
			Query:          localSearchContractQuery,
			Limit:          localSearchMessagesPerSession,
			ConversationID: conversation.DerivedID(conversation.ProviderClaude, session, ""),
		}
		waitForLocalSearch(t, deadline, scoped, func(result conversation.SearchConversationsResult) bool {
			return len(result.Matches) == localSearchMessagesPerSession
		}, "both messages of "+session)
	}
	archivedProbe := conversation.SearchConversationsOptions{
		Query:           localSearchContractQuery,
		Limit:           localSearchArchivedProbeLimit,
		Offset:          localSearchArchivedProbeOffset,
		IncludeArchived: true,
	}
	waitForLocalSearch(t, deadline, archivedProbe, func(result conversation.SearchConversationsResult) bool {
		if len(result.Matches) != localSearchArchivedProbeLimit {
			return false
		}
		for _, match := range result.Matches {
			if !match.Record.Archived {
				return false
			}
		}
		return true
	}, "the archived rows")

	visibleRows := len(sessions) * localSearchMessagesPerSession

	t.Run("default search returns the visible conversations ranked below the archived rows", func(t *testing.T) {
		result := searchLocalContract(t, conversation.SearchConversationsOptions{Query: localSearchContractQuery, Limit: visibleRows})
		if len(result.Matches) != visibleRows {
			t.Fatalf("default search returned %d matches, want the %d visible rows ranked below %d archived rows", len(result.Matches), visibleRows, localSearchArchivedRows)
		}
		for _, match := range result.Matches {
			if match.Record.Archived || match.Record.Provider != conversation.ProviderClaude {
				t.Fatalf("default search returned %s (archived %t), want only visible conversations", match.Record.ID, match.Record.Archived)
			}
		}
	})

	t.Run("every page is a slice of one ranking", func(t *testing.T) {
		const wide, narrow = 10, 3
		ranking := localSearchMatchKeys(searchLocalContract(t, conversation.SearchConversationsOptions{Query: localSearchContractQuery, Limit: wide}).Matches)
		if len(ranking) != wide {
			t.Fatalf("limit %d returned %d matches, want %d", wide, len(ranking), wide)
		}
		prefix := localSearchMatchKeys(searchLocalContract(t, conversation.SearchConversationsOptions{Query: localSearchContractQuery, Limit: narrow}).Matches)
		if fmt.Sprint(prefix) != fmt.Sprint(ranking[:narrow]) {
			t.Fatalf("limit %d returned %v, want the first %d items of limit %d: %v", narrow, prefix, narrow, wide, ranking[:narrow])
		}
		second := localSearchMatchKeys(searchLocalContract(t, conversation.SearchConversationsOptions{Query: localSearchContractQuery, Limit: narrow, Offset: narrow}).Matches)
		if fmt.Sprint(second) != fmt.Sprint(ranking[narrow:2*narrow]) {
			t.Fatalf("offset %d limit %d returned %v, want items %d through %d of limit %d: %v", narrow, narrow, second, narrow+1, 2*narrow, wide, ranking[narrow:2*narrow])
		}
	})

	t.Run("a filter admitting no conversation returns an empty result", func(t *testing.T) {
		result := searchLocalContract(t, conversation.SearchConversationsOptions{
			Query:         localSearchContractQuery,
			Limit:         visibleRows,
			WorkspaceRoot: localSearchMissingWorkspace,
		})
		if len(result.Matches) != 0 || result.HasMore {
			t.Fatalf("a workspace with no conversation returned %d matches and HasMore %t, want an empty result", len(result.Matches), result.HasMore)
		}
	})
}
