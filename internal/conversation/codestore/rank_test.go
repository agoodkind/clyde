package codestore_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	rankingRows          = 20000
	rankingConversations = 160
	rankingSentences     = 343
	rankingUpsertBatch   = 2000
	rankingQuery         = "why does the daemon rebind the listener after a reload"
)

var rankingWords = []string{"daemon", "listener", "rebind", "reload", "sourdough", "oven", "socket"}

func rankingSentence(position int) string {
	choice := position % rankingSentences
	first := rankingWords[choice%len(rankingWords)]
	second := rankingWords[(choice/len(rankingWords))%len(rankingWords)]
	third := rankingWords[(choice/(len(rankingWords)*len(rankingWords)))%len(rankingWords)]
	return strings.Join([]string{"the", first, "and the", second, "with the", third}, " ")
}

func rankingConversation(position int) string {
	return fmt.Sprintf("claude:%03d", (position*7)%rankingConversations)
}

func rankingSearch(t *testing.T, store *codestore.Store, vector []float32, limit int32, perGroup int32, minScore float64, filter *collection.Filter) []collection.Hit {
	t.Helper()
	request := collection.SearchRequest{
		Collection:    testCollection,
		Query:         rankingQuery,
		Vector:        vector,
		Limit:         limit,
		MinScore:      minScore,
		Filter:        filter,
		GroupBy:       "",
		PerGroupLimit: 0,
		Declaration:   declaration(),
	}
	if perGroup > 0 {
		request.GroupBy = itemColumn
		request.PerGroupLimit = perGroup
	}
	hits, err := store.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

func hitIDs(hits []collection.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

func walkRanking(ranking []collection.Hit, limit int, perGroup int, minScore float64, allowed map[string]bool) []string {
	counts := make(map[string]int)
	ids := make([]string, 0, limit)
	for _, hit := range ranking {
		if len(ids) == limit || hit.Score < minScore {
			break
		}
		conversationID := hit.Scalars[itemColumn].Value.String
		if allowed != nil && !allowed[conversationID] {
			continue
		}
		if perGroup > 0 && counts[conversationID] >= perGroup {
			continue
		}
		counts[conversationID]++
		ids = append(ids, hit.ID)
	}
	return ids
}

func TestSearchWalksOneExactRankingOverEveryAllowedRow(t *testing.T) {
	ctx := context.Background()
	model, err := staticembed.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	store, err := codestore.Open(t.TempDir(), staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}
	if err := store.EnsureCollection(ctx, ensure); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	batch := make([]collection.Row, 0, rankingUpsertBatch)
	for position := range rankingRows {
		batch = append(batch, testRow(t, model, fmt.Sprintf("row-%05d", (position*7919)%rankingRows), rankingConversation(position), rankingSentence(position)))
		if len(batch) == rankingUpsertBatch {
			if err := store.Upsert(ctx, testCollection, declaration(), batch); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			batch = batch[:0]
		}
	}

	vector := model.Vector(rankingQuery)
	ranking := rankingSearch(t, store, vector, rankingRows+1, 0, -1, nil)
	if len(ranking) != rankingRows {
		t.Fatalf("the full ranking has %d rows, want all %d", len(ranking), rankingRows)
	}
	ordered := slices.IsSortedFunc(ranking, func(left, right collection.Hit) int {
		if left.Score != right.Score {
			if left.Score > right.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(left.ID, right.ID)
	})
	if !ordered {
		t.Fatal("the full ranking is not ordered by descending score and ascending row ID")
	}

	allowed := make(map[string]bool)
	allowedValues := make([]string, 0, rankingConversations/2)
	for conversation := 0; conversation < rankingConversations; conversation += 2 {
		conversationID := fmt.Sprintf("claude:%03d", conversation)
		allowed[conversationID] = true
		allowedValues = append(allowedValues, conversationID)
	}
	allowedFilter := collection.ColumnIn(itemColumn, collection.StringValues(allowedValues))
	medianScore := ranking[rankingRows/2].Score
	bestScore := ranking[0].Score

	cases := []struct {
		name     string
		limit    int
		perGroup int
		minScore float64
		filter   *collection.Filter
		allowed  map[string]bool
	}{
		{name: "small window", limit: 7, perGroup: 0, minScore: -1, filter: nil, allowed: nil},
		{name: "window past the old ranking depth", limit: 16400, perGroup: 0, minScore: -1, filter: nil, allowed: nil},
		{name: "per-conversation cap", limit: 25, perGroup: 2, minScore: -1, filter: nil, allowed: nil},
		{name: "cap leaves fewer rows than the window", limit: 300, perGroup: 1, minScore: -1, filter: nil, allowed: nil},
		{name: "allowed set", limit: 40, perGroup: 0, minScore: -1, filter: &allowedFilter, allowed: allowed},
		{name: "allowed set with cap and minimum score", limit: 500, perGroup: 3, minScore: medianScore, filter: &allowedFilter, allowed: allowed},
		{name: "minimum score at the best score", limit: 50, perGroup: 1, minScore: bestScore, filter: nil, allowed: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := hitIDs(rankingSearch(t, store, vector, int32(testCase.limit), int32(testCase.perGroup), testCase.minScore, testCase.filter))
			want := walkRanking(ranking, testCase.limit, testCase.perGroup, testCase.minScore, testCase.allowed)
			if len(want) == 0 {
				t.Fatalf("the expected walk selects no rows")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Search returned %d rows starting %v, want %d rows starting %v", len(got), got[:min(len(got), 5)], len(want), want[:min(len(want), 5)])
			}
		})
	}
}
