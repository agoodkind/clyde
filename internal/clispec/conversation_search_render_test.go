package clispec

import (
	"strings"
	"testing"
	"time"

	conv "goodkind.io/clyde/internal/conversation"
)

// TestFormatSearchConversationsResultCleanList proves the search render is a
// scannable numbered list led by the [workspace] tag, and that the diagnostic
// fields (freshness, facets, filter funnel, raw score, per-record columns) stay
// out of the default text output.
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
		`Found 1 conversations for "daemon reload"`,
		"1. [/repo/alpha]  Add daemon reload command (claude)",
		"message 82",
		"Rank 1",
		"I meant the reload should force a new binary",
		"1 results.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"freshness:", "facets:", "filters:", "conversation_id", "0.0163"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("output should not contain diagnostic %q:\n%s", unwanted, out)
		}
	}
}

// TestFormatSearchConversationsResultEmpty proves the no-result path names the
// query and emits nothing else.
func TestFormatSearchConversationsResultEmpty(t *testing.T) {
	t.Parallel()
	out := formatSearchConversationsResult(conv.SearchConversationsResult{}, "nothing here")
	if out != "No conversations found for \"nothing here\".\n" {
		t.Fatalf("empty render = %q", out)
	}
}
