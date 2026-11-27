package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
)

type milvusSearchWithCoverage struct {
	*fakeSemanticSearch
	embedded map[string]bool
	readErr  error
	missing  bool
}

func (client *milvusSearchWithCoverage) SearchCollectionExists(
	context.Context,
	string,
) (bool, error) {
	return !client.missing, nil
}

func (client *milvusSearchWithCoverage) CoveredHits(
	_ context.Context,
	_ string,
	hits []semsearch.SemHit,
) ([]bool, error) {
	if client.readErr != nil {
		return nil, client.readErr
	}

	covered := make([]bool, len(hits))
	for index, hit := range hits {
		covered[index] = client.embedded[hit.Content]
	}

	return covered, nil
}

func newMilvusFirstTestSource(
	milvus conversationSemanticSearchClient,
	local conversationSemanticSearchClient,
) *backendConversationSearchSource {
	searchIndex := &fakeSearchIndex{
		records: map[string]conversation.Record{
			"claude:test": daemonTestRecord("claude:test", false),
		},
	}
	settings := config.ConversationSemanticConfig{
		SearchEnabled: true,
		CollectionID:  "conversations",
	}
	client := func(backend config.ConversationSemanticBackend) (
		conversationSemanticSearchClient,
		bool,
	) {
		if backend == config.ConversationSemanticBackendMilvus {
			return milvus, true
		}

		return local, true
	}

	return &backendConversationSearchSource{
		configured: &milvusFirstSearchSource{
			index:    searchIndex,
			settings: settings,
			client:   client,
		},
		index:    searchIndex,
		settings: func() config.ConversationSemanticConfig { return settings },
		client:   client,
	}
}

func dualSearchTestHit(messageIndex int32, content string) semsearch.SemHit {
	return semsearch.SemHit{
		ConversationID: "claude:test",
		MessageIndex:   messageIndex,
		Content:        content,
	}
}

func TestMilvusFirstSearchPagesPastCoveredLocalPassages(t *testing.T) {
	milvus := &milvusSearchWithCoverage{
		fakeSemanticSearch: &fakeSemanticSearch{
			hits: []semsearch.SemHit{dualSearchTestHit(1, "Milvus result")},
		},
		embedded: map[string]bool{"Milvus result": true, "Covered below threshold": true},
	}
	local := &fakeSemanticSearch{}

	for index := range int32(13) {
		local.hits = append(local.hits, dualSearchTestHit(index, "Covered below threshold"))
	}

	local.hits = append(
		local.hits,
		dualSearchTestHit(1, "Updated local content"),
		dualSearchTestHit(14, "New local content"),
	)
	source := newMilvusFirstTestSource(milvus, local)
	options := conversation.SearchConversationsOptions{
		Query:          "content",
		Limit:          2,
		ConversationID: "claude:test",
		MinScore:       0.25,
		Roles:          []string{"assistant"},
	}
	first, err := source.SearchConversations(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}

	if first.Source != conversation.SearchSourceMixed || len(first.Matches) != 2 ||
		first.Matches[0].Backend != conversation.SearchBackendMilvus ||
		first.Matches[1].Backend != conversation.SearchBackendLocal ||
		first.Matches[1].Snippet != "Updated local content" || !first.HasMore {
		t.Fatalf("first page = %+v", first)
	}

	if len(local.limits) < 2 {
		t.Fatalf("local limits = %v, want retries after the first covered window", local.limits)
	}

	for _, filter := range append(milvus.filters, local.filters...) {
		if filter.MinScore != options.MinScore || !slices.Equal(filter.Roles, options.Roles) ||
			!slices.Equal(filter.ConversationIDs, []string{"claude:test"}) {
			t.Fatalf("backend filter = %+v", filter)
		}
	}

	options.Offset = first.NextOffset
	second, err := source.SearchConversations(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}

	if second.Source != conversation.SearchSourceLocal || len(second.Matches) != 1 ||
		second.Matches[0].Snippet != "New local content" || second.HasMore {
		t.Fatalf("second page = %+v", second)
	}

	options.Offset = 0
	options.Backend = conversation.SearchBackendLocal
	explicit, err := source.SearchConversations(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}

	if explicit.Source != conversation.SearchSourceLocal || len(explicit.Matches) != 2 ||
		explicit.Matches[0].Snippet != "Covered below threshold" {
		t.Fatalf("explicit local search = %+v", explicit)
	}
}

func TestMilvusFirstSearchFallbackErrors(t *testing.T) {
	for _, test := range []struct {
		name      string
		primary   error
		coverage  error
		wantLocal bool
	}{
		{
			name:      "search unavailable",
			primary:   status.Error(codes.Unavailable, "Milvus disconnected"),
			wantLocal: true,
		},
		{
			name:      "coverage unavailable",
			coverage:  errors.New("Milvus disconnected"),
			wantLocal: true,
		},
		{name: "caller cancelled", primary: context.Canceled},
		{name: "invalid request", primary: status.Error(codes.InvalidArgument, "query refused")},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				milvus := &milvusSearchWithCoverage{
					fakeSemanticSearch: &fakeSemanticSearch{err: test.primary},
					readErr:            test.coverage,
				}
				local := &fakeSemanticSearch{
					hits: []semsearch.SemHit{dualSearchTestHit(1, "Local result")},
				}
				source := newMilvusFirstTestSource(milvus, local)
				result, err := source.SearchConversations(
					t.Context(),
					conversation.SearchConversationsOptions{
						Query: "content",
						Limit: 2,
					},
				)
				if !test.wantLocal {
					if err == nil || len(local.limits) != 0 {
						t.Fatalf("search error = %v, local limits = %v", err, local.limits)
					}

					return
				}

				if err != nil || result.Source != conversation.SearchSourceLocal ||
					len(result.Matches) != 1 {
					t.Fatalf("search result = %+v, error = %v", result, err)
				}
			},
		)
	}
}

func TestMilvusFirstSearchSkipsQueryEmbeddingForMissingCollection(t *testing.T) {
	milvus := &milvusSearchWithCoverage{fakeSemanticSearch: &fakeSemanticSearch{}, missing: true}
	local := &fakeSemanticSearch{hits: []semsearch.SemHit{dualSearchTestHit(1, "Local result")}}
	source := newMilvusFirstTestSource(milvus, local)

	result, err := source.SearchConversations(
		t.Context(),
		conversation.SearchConversationsOptions{
			Query: "content",
			Limit: 2,
		},
	)
	if err != nil || result.Source != conversation.SearchSourceLocal || len(milvus.limits) != 0 {
		t.Fatalf("result = %+v, error = %v, Milvus calls = %v", result, err, milvus.limits)
	}
}

func TestMilvusFirstSearchKeepsPrimaryMatchesWhenLocalIsUnavailable(t *testing.T) {
	milvus := &milvusSearchWithCoverage{
		fakeSemanticSearch: &fakeSemanticSearch{
			hits: []semsearch.SemHit{dualSearchTestHit(1, "Milvus result")},
		},
	}
	source := newMilvusFirstTestSource(milvus, nil)
	result, err := source.SearchConversations(
		t.Context(),
		conversation.SearchConversationsOptions{
			Query: "content",
			Limit: 2,
		},
	)
	if err != nil || result.Source != conversation.SearchSourceSemantic ||
		len(result.Matches) != 1 {
		t.Fatalf("search result = %+v, error = %v", result, err)
	}
}
