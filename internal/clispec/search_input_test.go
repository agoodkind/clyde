package clispec

import (
	"testing"
)

// TestSplitRolesDropsEmptyEntries proves the comma-separated roles flag parses
// into a clean role set.
func TestSplitRolesDropsEmptyEntries(t *testing.T) {
	t.Parallel()

	roles := splitRoles(" user, ,assistant,")
	if len(roles) != 2 || roles[0] != "user" || roles[1] != "assistant" {
		t.Fatalf("roles = %v, want [user assistant]", roles)
	}
	if splitRoles("  ") != nil {
		t.Fatal("blank roles input did not return nil")
	}
}

// TestTimeBoundUnixParsesAcceptedLayouts proves the after and until flags
// accept the documented spellings, return zero for empty, and reject garbage.
func TestTimeBoundUnixParsesAcceptedLayouts(t *testing.T) {
	t.Parallel()

	zero, err := timeBoundUnix("", "after")
	if err != nil || zero != 0 {
		t.Fatalf("empty bound = (%d, %v), want (0, nil)", zero, err)
	}
	for _, spelling := range []string{"2026-06-09T12:00:00Z", "2026-06-09 12:00", "2026-06-09"} {
		parsed, parseErr := timeBoundUnix(spelling, "after")
		if parseErr != nil || parsed <= 0 {
			t.Fatalf("bound %q = (%d, %v), want a positive unix time", spelling, parsed, parseErr)
		}
	}
	if _, err := timeBoundUnix("next tuesday", "until"); err == nil {
		t.Fatal("garbage bound parsed without error")
	}
}

// TestPrepareSearchUsesConversationIDPositional proves the optional
// conversation_id positional scopes search and read modes.
func TestPrepareSearchUsesConversationIDPositional(t *testing.T) {
	t.Parallel()

	searchPayload, err := prepareSearch(searchInput{
		Query:          "auth timeout",
		ConversationID: "claude:abc",
		Limit:          20,
		Window:         5,
		Around:         -1,
	})
	if err != nil {
		t.Fatalf("prepare scoped search: %v", err)
	}
	if searchPayload.Mode != searchModeDiscover {
		t.Fatalf("mode = %v, want discover", searchPayload.Mode)
	}
	if searchPayload.SearchOpts.ConversationID != "claude:abc" {
		t.Fatalf("conversation id = %q, want claude:abc", searchPayload.SearchOpts.ConversationID)
	}

	readPayload, err := prepareSearch(searchInput{
		ConversationID: "claude:abc",
		Limit:          20,
		Window:         5,
		Around:         -1,
	})
	if err != nil {
		t.Fatalf("prepare read: %v", err)
	}
	if readPayload.Mode != searchModeReadConversation {
		t.Fatalf("mode = %v, want read conversation", readPayload.Mode)
	}
}
