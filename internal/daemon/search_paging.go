package daemon

import (
	"context"

	"goodkind.io/clyde/internal/conversation"
)

const maxInt32Value = 2147483647

type invalidSearchBoundsError string

func (e invalidSearchBoundsError) Error() string {
	return string(e)
}

func normalizedPagingOffset(rawOffset int) int {
	if rawOffset < 0 {
		return 0
	}
	if rawOffset > maxInt32Value {
		return maxInt32Value
	}
	return rawOffset
}

func normalizedSearchLimit(rawLimit int) int {
	if rawLimit <= 0 {
		return conversation.DefaultSearchLimit
	}
	if rawLimit > conversation.MaxSearchLimit {
		return conversation.MaxSearchLimit
	}
	return rawLimit
}

func semanticSearchPageBounds(options conversation.SearchConversationsOptions) (int, int, int, error) {
	limit := normalizedSearchLimit(options.Limit)
	normalizedOffset := int64(normalizedPagingOffset(options.Offset))
	searchLimit := int64(limit) + normalizedOffset
	if normalizedOffset > maxInt32Value || searchLimit > maxInt32Value || searchLimit < int64(limit) {
		return 0, 0, 0, invalidSearchBoundsError("offset too large")
	}
	return limit, int(normalizedOffset), int(searchLimit), nil
}

func semanticSearchResult(
	ctx context.Context,
	index conversationSearchIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	accounting := filterAccounting(ctx, index, options)
	normalizedLimit := normalizedSearchLimit(options.Limit)
	normalizedOffset := normalizedPagingOffset(options.Offset)
	matches, err := engineSearchMatches(ctx, index, semantic, collectionID, options)
	if err != nil {
		return conversation.SearchConversationsResult{}, err
	}
	return conversation.SearchConversationsResult{
		Matches:              matches,
		ConversationsScanned: len(matches),
		ReturnedCount:        len(matches),
		Limit:                normalizedLimit,
		Offset:               normalizedOffset,
		NextOffset:           normalizedOffset + len(matches),
		HasMore:              len(matches) >= normalizedLimit && normalizedLimit > 0,
		Source:               conversation.SearchSourceSemantic,
		Facets:               conversation.ComputeFacets(matches, searchFacetTopN),
		Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting:     appendReturnedStage(accounting, len(matches)),
	}, nil
}
