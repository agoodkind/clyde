package clispec

import (
	"strings"
	"testing"
	"time"

	conv "goodkind.io/clyde/internal/conversation"
)

// TestFormatSearchConversationsResultCleanList proves the search render is a
// scannable numbered list led by the [workspace] tag, shows the message date
// with hour and minute, and keeps internal numbers (message index, rank) and the
// diagnostic fields (freshness, facets, filter funnel, raw score, per-record
// columns) out of the default text output.
func TestFormatSearchConversationsResultCleanList(t *testing.T) {
	t.Parallel()
	result := conv.SearchConversationsResult{
		Matches: []conv.SearchMatch{
			{
				Record: conv.Record{
					ID:            "claude:abc",
					Provider:      conv.ProviderClaude,
					Title:         "Add daemon reload command",
					WorkspaceRoot: "/repo/alpha",
					Archived:      false,
				},
				MessageIndex: 82,
				Role:         "user",
				Timestamp:    time.Date(2026, 4, 26, 13, 53, 48, 0, time.UTC),
				Snippet:      "I meant the reload should\nforce a new binary",
				Score:        0.0163,
			},
		},
		ReturnedCount: 1,
		Limit:         10,
		Source:        conv.SearchSourceSemantic,
		Freshness:     conv.SearchFreshness{Manifest: 1539, Needed: 66, Embedded: 1473, Pending: 0, LastSyncUnix: 0},
		Facets:        conv.SearchFacets{Providers: []conv.SearchFacetCount{{Value: "claude", Count: 1}}},
	}

	out := formatSearchConversationsResult(result, "daemon reload")

	for _, want := range []string{
		`Found 1 results for "daemon reload"`,
		"1. [/repo/alpha]  Add daemon reload command (claude)",
		"2026-04-26 13:53 · user",
		"I meant the reload should force a new binary",
		"1 results.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// No internal numbers, no diagnostic chrome, and no archived marker on a
	// conversation that is not archived.
	for _, unwanted := range []string{"message 82", "Rank 1", "Rank", "freshness:", "facets:", "filters:", "conversation_id", "0.0163", "archived"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("output should not contain %q:\n%s", unwanted, out)
		}
	}
}

// TestFormatSearchConversationsResultArchivedMarker proves the archived marker
// appears only when the conversation is archived.
func TestFormatSearchConversationsResultArchivedMarker(t *testing.T) {
	t.Parallel()
	result := conv.SearchConversationsResult{
		Matches: []conv.SearchMatch{
			{
				Record: conv.Record{
					ID:            "claude:old",
					Provider:      conv.ProviderClaude,
					Title:         "Old session",
					WorkspaceRoot: "/repo/alpha",
					Archived:      true,
				},
				Role:      "assistant",
				Timestamp: time.Date(2026, 3, 1, 9, 5, 0, 0, time.UTC),
				Snippet:   "an old answer",
			},
		},
		ReturnedCount: 1,
	}

	out := formatSearchConversationsResult(result, "old")

	if !strings.Contains(out, "2026-03-01 09:05 · assistant · archived") {
		t.Fatalf("archived conversation should show the archived marker:\n%s", out)
	}
}

// TestFormatSearchConversationsResultEmpty proves the no-result path names the
// query and emits nothing else.
func TestFormatSearchConversationsResultEmpty(t *testing.T) {
	t.Parallel()
	out := formatSearchConversationsResult(conv.SearchConversationsResult{}, "nothing here")
	if out != "No results found for \"nothing here\".\n" {
		t.Fatalf("empty render = %q", out)
	}
}
