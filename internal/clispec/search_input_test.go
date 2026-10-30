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

func TestTimeBoundUnixParsesAcceptedLayouts(t *testing.T) {
	t.Parallel()

	omitted, err := timeBoundUnix("", "after")
	if err != nil || omitted != nil {
		t.Fatalf("An omitted bound should return nil without an error. Result and error: %v, %v", omitted, err)
	}
	epoch, err := timeBoundUnix("1970-01-01T00:00:00Z", "after")
	if err != nil || epoch == nil || *epoch != 0 {
		t.Fatalf("The epoch bound should return a pointer to zero without an error. Result and error: %v, %v", epoch, err)
	}
	for _, spelling := range []string{"2026-06-09T12:00:00Z", "2026-06-09 12:00", "2026-06-09"} {
		parsed, parseErr := timeBoundUnix(spelling, "after")
		if parseErr != nil || parsed == nil || *parsed <= 0 {
			t.Fatalf("The accepted date should return a positive Unix timestamp without an error. Input, result, and error: %q, %v, %v", spelling, parsed, parseErr)
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

// TestPrepareSearchCarriesOffset proves paging applies to search and browse
// modes from the shared --offset flag.
func TestPrepareSearchCarriesOffset(t *testing.T) {
	t.Parallel()

	searchPayload, err := prepareSearch(searchInput{
		Query:  "auth timeout",
		Limit:  20,
		Offset: 40,
		Window: 5,
		Around: -1,
	})
	if err != nil {
		t.Fatalf("prepare search: %v", err)
	}
	if searchPayload.SearchOpts.Offset != 40 {
		t.Fatalf("search offset = %d, want 40", searchPayload.SearchOpts.Offset)
	}

	browsePayload, err := prepareSearch(searchInput{
		Limit:  20,
		Offset: 40,
		Window: 5,
		Around: -1,
	})
	if err != nil {
		t.Fatalf("prepare browse: %v", err)
	}
	if browsePayload.ListOpts.Offset != 40 {
		t.Fatalf("browse offset = %d, want 40", browsePayload.ListOpts.Offset)
	}
}

// TestPrepareSearchCarriesCursorOnlyWithQuery proves prepareSearch copies the
// trimmed --cursor value into the search options for a query and rejects a
// cursor in read and browse modes, which have no search to continue.
func TestPrepareSearchCarriesCursorOnlyWithQuery(t *testing.T) {
	t.Parallel()

	searchPayload, err := prepareSearch(searchInput{
		Query:  "auth timeout",
		Cursor: " page-two ",
		Limit:  20,
		Window: 5,
		Around: -1,
	})
	if err != nil {
		t.Fatalf("prepare search: %v", err)
	}
	if searchPayload.SearchOpts.Cursor != "page-two" {
		t.Fatalf("search cursor = %q, want page-two", searchPayload.SearchOpts.Cursor)
	}

	if _, err := prepareSearch(searchInput{
		ConversationID: "claude:abc",
		Cursor:         "page-two",
		Limit:          20,
		Window:         5,
		Around:         -1,
	}); err == nil {
		t.Fatal("prepare read with a cursor succeeded, want an error")
	}
	if _, err := prepareSearch(searchInput{
		Cursor: "page-two",
		Limit:  20,
		Window: 5,
		Around: -1,
	}); err == nil {
		t.Fatal("prepare browse with a cursor succeeded, want an error")
	}
}
