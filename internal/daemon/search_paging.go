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
	page, err := engineSearchMatches(ctx, index, semantic, collectionID, options)
	if err != nil {
		return conversation.SearchConversationsResult{}, err
	}
	matches := page.matches
	return conversation.SearchConversationsResult{
		Matches:              matches,
		ConversationsScanned: len(matches),
		ReturnedCount:        len(matches),
		Limit:                normalizedLimit,
		Offset:               normalizedOffset,
		NextOffset:           normalizedOffset + len(matches),
		// HasMore is true only on a page with at least one row. NextOffset is
		// then greater than the request offset, and a caller that follows it
		// never requests the same page again. A short page with rows reports
		// HasMore because ranked hits remain after the over-fetch budget ran out.
		HasMore:          len(matches) > 0 && (len(matches) >= normalizedLimit || page.short),
		NextCursor:       "",
		Source:           conversation.SearchSourceSemantic,
		Facets:           conversation.ComputeFacets(matches, searchFacetTopN),
		Freshness:        conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting: appendReturnedStage(appendWithheldStages(accounting, page), len(matches)),
	}, nil
}

// appendWithheldStages records the ranked hits the engine returned and how many
// of them survived hiding, so a caller reading the funnel can see that ranked
// matches were dropped rather than never found. Nothing is appended when every
// ranked hit resolved, which keeps the common funnel unchanged.
func appendWithheldStages(stages []conversation.FilterStage, page engineSearchPage) []conversation.FilterStage {
	if page.withheld == 0 {
		return stages
	}
	stages = append(stages, conversation.FilterStage{Name: "engine_ranked", Remaining: page.ranked})
	return append(stages, conversation.FilterStage{Name: "hidden_excluded", Remaining: page.ranked - page.withheld})
}
