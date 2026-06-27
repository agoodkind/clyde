package daemon

import (
	"context"
	"log/slog"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

const maxInt32Value = 2147483647

type invalidSearchBoundsError string

func (e invalidSearchBoundsError) Error() string {
	return string(e)
}

func normalizedPagingOffset(rawOffset int64) int {
	if rawOffset < 0 {
		return 0
	}
	if rawOffset > maxInt32Value {
		return maxInt32Value
	}
	return int(rawOffset)
}

func normalizedSearchLimit(rawLimit int64) int {
	if rawLimit <= 0 {
		return conversation.DefaultSearchLimit
	}
	if rawLimit > conversation.MaxSearchLimit {
		return conversation.MaxSearchLimit
	}
	return int(rawLimit)
}

func semanticSearchPageBounds(req *clydev1.SearchConversationsRequest) (int, int, int, error) {
	limit := normalizedSearchLimit(req.GetLimit())
	normalizedOffset := int64(normalizedPagingOffset(req.GetOffset()))
	searchLimit := int64(limit) + normalizedOffset
	if normalizedOffset > maxInt32Value || searchLimit > maxInt32Value || searchLimit < int64(limit) {
		return 0, 0, 0, invalidSearchBoundsError("offset too large")
	}
	return limit, int(normalizedOffset), int(searchLimit), nil
}

func semanticSearchResult(
	ctx context.Context,
	idx searchConversationsIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	literalFallback bool,
	req *clydev1.SearchConversationsRequest,
	accounting []conversation.FilterStage,
	normalizedLimit int,
	normalizedOffset int,
) (conversation.SearchConversationsResult, bool, error) {
	matches, err := engineSearchMatches(ctx, idx, semantic, collectionID, req)
	if err != nil {
		return conversation.SearchConversationsResult{}, false, err
	}
	if len(matches) >= 1 {
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
		}, true, nil
	}
	if normalizedOffset > 0 {
		semanticAny, probeErr := semanticHasAnyMatches(ctx, idx, semantic, collectionID, req)
		if probeErr != nil {
			return conversation.SearchConversationsResult{}, false, probeErr
		}
		if semanticAny {
			return emptySemanticPageResult(normalizedLimit, normalizedOffset, accounting), true, nil
		}
	}
	if !literalFallback {
		slog.WarnContext(ctx, "daemon.search_conversations.literal_disabled_cold", "concern", "process.daemon.lifecycle", "component", "daemon",
			"query", req.GetQuery(),
		)
		return conversation.SearchConversationsResult{
			Matches:              nil,
			ConversationsScanned: 0,
			ReturnedCount:        0,
			Limit:                normalizedLimit,
			Offset:               normalizedOffset,
			NextOffset:           normalizedOffset,
			HasMore:              false,
			Source:               conversation.SearchSourceLiteralDisabledCold,
			Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
			Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
			FilterAccounting:     appendReturnedStage(accounting, 0),
		}, true, nil
	}
	var empty conversation.SearchConversationsResult
	return empty, false, nil
}

func emptySemanticPageResult(normalizedLimit, normalizedOffset int, accounting []conversation.FilterStage) conversation.SearchConversationsResult {
	return conversation.SearchConversationsResult{
		Matches:              nil,
		ConversationsScanned: 0,
		ReturnedCount:        0,
		Limit:                normalizedLimit,
		Offset:               normalizedOffset,
		NextOffset:           normalizedOffset,
		HasMore:              false,
		Source:               conversation.SearchSourceSemantic,
		Facets:               conversation.SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
		Freshness:            conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting:     appendReturnedStage(accounting, 0),
	}
}

func semanticHasAnyMatches(
	ctx context.Context,
	idx searchConversationsIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	req *clydev1.SearchConversationsRequest,
) (bool, error) {
	probe := semanticMatchProbeRequest(req)
	matches, err := engineSearchMatches(ctx, idx, semantic, collectionID, probe)
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func semanticMatchProbeRequest(req *clydev1.SearchConversationsRequest) *clydev1.SearchConversationsRequest {
	return &clydev1.SearchConversationsRequest{
		Query:                req.GetQuery(),
		Limit:                int64(conversation.MaxSearchLimit),
		Offset:               0,
		Provider:             req.GetProvider(),
		Workspace:            req.GetWorkspace(),
		IncludeArchived:      req.GetIncludeArchived(),
		Roles:                req.GetRoles(),
		FromUnix:             req.GetFromUnix(),
		UntilUnix:            req.GetUntilUnix(),
		MinScore:             req.GetMinScore(),
		PerConversationLimit: req.GetPerConversationLimit(),
		ConversationId:       req.GetConversationId(),
		ContextWindow:        req.GetContextWindow(),
	}
}
