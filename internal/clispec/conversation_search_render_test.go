package clispec

import (
	"strings"
	"testing"
	"time"

	conv "goodkind.io/clyde/internal/conversation"
)

func TestFormatSearchConversationsResultUsesSemanticSearchLayout(t *testing.T) {
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
	want := "🔍 Found 1 conversation result for query: \"daemon reload\" in collection 'clyde-conversations'\n\n" +
		"1. Conversation message [clyde-conversations]\n" +
		"   Conversation: claude:abc\n" +
		"   Message index: 82\n" +
		"   Role: user\n" +
		"   Timestamp: 2026-04-26 13:53 UTC\n" +
		"   Rank: 1\n" +
		"   Content:\n" +
		"```\n" +
		"I meant the reload should\nforce a new binary\n" +
		"```\n"
	if out != want {
		t.Fatalf("search output mismatch:\nwant:\n%s\ngot:\n%s", want, out)
	}
}

func TestFormatSearchConversationsResultUsesFenceLongerThanSnippet(t *testing.T) {
	t.Parallel()
	result := conv.SearchConversationsResult{
		Matches: []conv.SearchMatch{
			{
				Record: conv.Record{
					ID:       "claude:abc",
					Provider: conv.ProviderClaude,
				},
				MessageIndex: 82,
				Role:         "assistant",
				Timestamp:    time.Date(2026, 4, 26, 13, 53, 48, 0, time.UTC),
				Snippet:      "before\n```go\nfmt.Println(\"hi\")\n```\nafter",
			},
		},
		ReturnedCount: 1,
		Limit:         1,
	}

	out := formatSearchConversationsResult(result, "fence")
	want := "   Content:\n````\n" +
		"before\n```go\nfmt.Println(\"hi\")\n```\nafter\n" +
		"````\n"
	if !strings.Contains(out, want) {
		t.Fatalf("fenced snippet output missing:\n%s\ngot:\n%s", want, out)
	}
}

func TestFormatSearchConversationsResultEmpty(t *testing.T) {
	t.Parallel()
	out := formatSearchConversationsResult(conv.SearchConversationsResult{}, "nothing here")
	if out != "🔍 No conversation results found for query: \"nothing here\" in collection 'clyde-conversations'\n" {
		t.Fatalf("empty render = %q", out)
	}
}

func TestFormatSearchConversationsResultShowsOffsetHint(t *testing.T) {
	t.Parallel()
	result := conv.SearchConversationsResult{
		Matches: []conv.SearchMatch{
			{
				Record: conv.Record{
					ID:            "claude:abc",
					Provider:      conv.ProviderClaude,
					Title:         "Auth timeout",
					WorkspaceRoot: "/repo/alpha",
				},
				Role:      "user",
				Timestamp: time.Date(2026, 4, 26, 13, 53, 48, 0, time.UTC),
				Snippet:   "auth timeout",
			},
		},
		ReturnedCount: 1,
		Limit:         1,
		NextOffset:    21,
		HasMore:       true,
	}

	out := formatSearchConversationsResult(result, "auth")
	want := "🔍 Found 1 conversation result for query: \"auth\" in collection 'clyde-conversations'\n\n" +
		"1. Conversation message [clyde-conversations]\n" +
		"   Conversation: claude:abc\n" +
		"   Message index: 0\n" +
		"   Role: user\n" +
		"   Timestamp: 2026-04-26 13:53 UTC\n" +
		"   Rank: 1\n" +
		"   Content:\n" +
		"```\n" +
		"auth timeout\n" +
		"```\n\n" +
		"More: --offset 21\n"
	if out != want {
		t.Fatalf("pagination output mismatch:\nwant:\n%s\ngot:\n%s", want, out)
	}
}
