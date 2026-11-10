//go:build milvusintegration

package localbackendcli_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	shapesPageSize           = 3
	shapesPageCount          = 3
	shapesFloorPosition      = 5
	shapesConversationLimit  = 2
	shapesConversationOffset = 2
)

// shapesDescribe returns each match as a conversation ID, message index, role, and score, joined by spaces.
func shapesDescribe(matches []localProofMatch) string {
	parts := make([]string, 0, len(matches))
	for _, match := range matches {
		parts = append(parts, fmt.Sprintf("%s/%d/%s=%.6f", match.Conversation.ID, match.MessageIndex, match.Role, match.Score))
	}
	return strings.Join(parts, " ")
}

// assertShapesScoreOrder fails the test when a match scores above the preceding match.
func assertShapesScoreOrder(t *testing.T, label string, matches []localProofMatch) {
	t.Helper()
	for position := 1; position < len(matches); position++ {
		if matches[position].Score > matches[position-1].Score {
			t.Errorf("%s: match %d scores %.6f above match %d at %.6f: %s", label, position, matches[position].Score, position-1, matches[position-1].Score, shapesDescribe(matches))
		}
	}
}

// assertShapesPageSizes checks that a limit 1 search returns the first match of the limit 9 ranking and that pages of 3 at offsets 0, 3, and 6 return that ranking in order.
func assertShapesPageSizes(t *testing.T, surface localProofSurface) {
	t.Helper()
	requests := []localProofSearchRequest{
		{Query: localProofQuery, Limit: shapesPageCount * shapesPageSize},
		{Query: localProofQuery, Limit: 1},
	}
	for page := range shapesPageCount {
		requests = append(requests, localProofSearchRequest{Query: localProofQuery, Limit: shapesPageSize, Offset: page * shapesPageSize})
	}
	results := localProofSearchAll(t, surface, requests)
	ranking := results[0].Matches
	t.Logf("%s limit %d: %s", surface.name, shapesPageCount*shapesPageSize, shapesDescribe(ranking))
	if len(ranking) != shapesPageCount*shapesPageSize {
		t.Fatalf("%s limit %d returned %d matches, want %d", surface.name, shapesPageCount*shapesPageSize, len(ranking), shapesPageCount*shapesPageSize)
	}
	assertShapesScoreOrder(t, surface.name+" ranking", ranking)
	if single := localProofMatchKeys(results[1].Matches); !slices.Equal(single, localProofMatchKeys(ranking[:1])) {
		t.Errorf("%s limit 1 returned %v, want %v", surface.name, single, localProofMatchKeys(ranking[:1]))
	}
	paged := make([]localProofMatch, 0, len(ranking))
	for page, result := range results[2:] {
		t.Logf("%s limit %d offset %d: %s", surface.name, shapesPageSize, page*shapesPageSize, shapesDescribe(result.Matches))
		if result.Source != localProofSourceSemantic || len(result.Matches) != shapesPageSize {
			t.Errorf("%s offset %d returned source %q and %d matches, want %q and %d", surface.name, page*shapesPageSize, result.Source, len(result.Matches), localProofSourceSemantic, shapesPageSize)
		}
		paged = append(paged, result.Matches...)
	}
	if !slices.Equal(localProofMatchKeys(paged), localProofMatchKeys(ranking)) {
		t.Errorf("%s pages of %d differ from the limit %d ranking:\npages   %s\nranking %s", surface.name, shapesPageSize, len(ranking), shapesDescribe(paged), shapesDescribe(ranking))
	}
}

// assertShapesConversationScope checks that a search scoped to one conversation returns only its 6 matches in score order and that limit 2 and offset 2 return the third and fourth matches.
func assertShapesConversationScope(t *testing.T, surface localProofSurface, conversationID string) {
	t.Helper()
	results := localProofSearchAll(t, surface, []localProofSearchRequest{
		{Query: localProofQuery, Limit: localProofMaxLimit, ConversationID: conversationID},
		{Query: localProofQuery, Limit: shapesConversationLimit, Offset: shapesConversationOffset, ConversationID: conversationID},
	})
	scoped, page := results[0].Matches, results[1].Matches
	t.Logf("%s conversation %s: %s", surface.name, conversationID, shapesDescribe(scoped))
	t.Logf("%s conversation %s limit %d offset %d: %s", surface.name, conversationID, shapesConversationLimit, shapesConversationOffset, shapesDescribe(page))
	if len(scoped) != localProofSessionRows {
		t.Fatalf("%s conversation %s returned %d matches, want %d: %s", surface.name, conversationID, len(scoped), localProofSessionRows, shapesDescribe(scoped))
	}
	for _, match := range slices.Concat(scoped, page) {
		if match.Conversation.ID != conversationID {
			t.Errorf("%s conversation %s returned a match from %s", surface.name, conversationID, match.Conversation.ID)
		}
	}
	assertShapesScoreOrder(t, surface.name+" conversation", scoped)
	want := localProofMatchKeys(scoped[shapesConversationOffset : shapesConversationOffset+shapesConversationLimit])
	if got := localProofMatchKeys(page); !slices.Equal(got, want) {
		t.Errorf("%s conversation %s offset %d returned %v, want %v", surface.name, conversationID, shapesConversationOffset, got, want)
	}
}

// assertShapesScoreFloor sets the score floor to the sixth match score and checks that the search returns exactly the unfloored matches that score at or above the floor.
func assertShapesScoreFloor(t *testing.T, surface localProofSurface) {
	t.Helper()
	full := mustLocalProofSearch(t, surface, localProofSearchRequest{Query: localProofQuery, Limit: localProofMaxLimit})
	if len(full.Matches) <= shapesFloorPosition {
		t.Fatalf("%s returned %d matches, want more than %d", surface.name, len(full.Matches), shapesFloorPosition)
	}
	floor := full.Matches[shapesFloorPosition].Score
	floored := mustLocalProofSearch(t, surface, localProofSearchRequest{Query: localProofQuery, Limit: localProofMaxLimit, MinScore: floor})
	t.Logf("%s min score %.6f: %s", surface.name, floor, shapesDescribe(floored.Matches))
	want := make([]localProofMatchKey, 0, len(full.Matches))
	for _, match := range full.Matches {
		if match.Score >= floor {
			want = append(want, localProofMatchKeys([]localProofMatch{match})...)
		}
	}
	for _, match := range floored.Matches {
		if match.Score < floor {
			t.Errorf("%s min score %.6f returned a match scoring %.6f", surface.name, floor, match.Score)
		}
	}
	if got := localProofMatchKeys(floored.Matches); !slices.Equal(got, want) {
		t.Errorf("%s min score %.6f returned %v, want %v", surface.name, floor, got, want)
	}
}

// TestMilvusSearchKeepsScopesPagesAndFloors runs the sandbox daemon with a throwaway Milvus and checks filters, offset pages, other page sizes, conversation scope, and score floors through CLI and MCP search.
func TestMilvusSearchKeepsScopesPagesAndFloors(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	baseURL := os.Getenv(deletedSourceEmbeddingEnv)
	if baseURL == "" {
		baseURL = deletedSourceEmbeddingDefault
	}
	settingsPath := writeSandboxSemanticSettings(t, fmt.Sprintf(withheldSettings, address, baseURL, deletedSourceEmbeddingModel, deletedSourceEmbeddingDim))
	binaryPath := buildClydeCLI(t)
	home := writeLocalProofHome(t)
	sandboxDaemon := startLocalProofDaemon(t, binaryPath, home, "--ingestion-enabled", "--search-enabled", "--semantic-settings", settingsPath)
	waitForProofRowsFrom(t, sandboxDaemon, localProofSourceSemantic)

	alphaID := localProofProviderClaude + ":" + localProofAlphaSession
	for _, surface := range sandboxDaemon.surfaces() {
		assertLocalProofFilters(t, surface, localProofSourceSemantic)
		assertLocalProofPages(t, surface, localProofSourceSemantic)
		assertShapesPageSizes(t, surface)
		assertShapesConversationScope(t, surface, alphaID)
		assertShapesScoreFloor(t, surface)
	}
}
