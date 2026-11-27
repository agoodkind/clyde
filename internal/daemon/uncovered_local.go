package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
)

// uncoveredLocalSearchClient removes passages already embedded in Milvus
// before the search source applies pagination.
type uncoveredLocalSearchClient struct {
	conversationSemanticSearchClient

	milvusCoverage searchCoverage
	primary        []conversation.SearchMatch
	checked        map[passageCoverageKey]bool
}

var errCoverageCountMismatch = errors.New("coverage decision count does not match local hit count")

type passageCoverageKey struct {
	conversationID string
	relativePath   string
	loadRules      string
	content        string
	splitPart      int32
}

func (client *uncoveredLocalSearchClient) RanksEveryAllowedPassage() bool {
	local, ok := client.conversationSemanticSearchClient.(exactRankingSearchClient)

	return ok && local.RanksEveryAllowedPassage()
}

func (client *uncoveredLocalSearchClient) SearchConversations(
	ctx context.Context,
	collectionID string,
	query string,
	limit int32,
	filter semsearch.SearchFilter,
	perConversationLimit int32,
) ([]semsearch.SemHit, error) {
	for candidateLimit := limit; ; candidateLimit = growLocalCandidateLimit(candidateLimit) {
		hits, err := client.conversationSemanticSearchClient.SearchConversations(
			ctx,
			collectionID,
			query,
			candidateLimit,
			filter,
			0,
		)
		if err != nil {
			slog.WarnContext(
				ctx,
				"daemon.search_conversations.local_passages_failed",
				"concern",
				"conversation.search",
				"component",
				"daemon",
				"code",
				sourceRPCCode(err).String(),
			)

			return nil, fmt.Errorf("search local passages: %w", err)
		}

		uncovered, err := client.uncoveredHits(ctx, collectionID, hits, limit, perConversationLimit)
		if err != nil {
			return nil, err
		}

		if len(uncovered) == int(limit) || len(hits) < int(candidateLimit) ||
			candidateLimit == maxInt32Value {
			return uncovered, nil
		}
	}
}

func growLocalCandidateLimit(limit int32) int32 {
	if limit > maxInt32Value/searchOverfetchFactor {
		return maxInt32Value
	}

	return limit * searchOverfetchFactor
}

func (client *uncoveredLocalSearchClient) uncoveredHits(
	ctx context.Context,
	collectionID string,
	hits []semsearch.SemHit,
	limit int32,
	perConversationLimit int32,
) ([]semsearch.SemHit, error) {
	covered, err := client.checkCoverage(ctx, collectionID, hits)
	if err != nil {
		return nil, searchCoverageError{cause: err}
	}

	used := make(map[string]int32)
	if perConversationLimit > 0 {
		for _, match := range client.primary {
			used[match.Record.ID]++
		}
	}

	uncovered := make([]semsearch.SemHit, 0, min(int(limit), len(hits)))
	for index, hit := range hits {
		if covered[index] {
			continue
		}

		if perConversationLimit > 0 && used[hit.ConversationID] >= perConversationLimit {
			continue
		}

		used[hit.ConversationID]++
		uncovered = append(uncovered, hit)
		if len(uncovered) == int(limit) {
			break
		}
	}

	return uncovered, nil
}

func (client *uncoveredLocalSearchClient) checkCoverage(
	ctx context.Context,
	collectionID string,
	hits []semsearch.SemHit,
) ([]bool, error) {
	if client.checked == nil {
		client.checked = make(map[passageCoverageKey]bool)
	}

	covered := make([]bool, len(hits))
	var pending []semsearch.SemHit
	var pendingIndices []int

	for index, hit := range hits {
		if value, ok := client.checked[coverageKey(hit)]; ok {
			covered[index] = value

			continue
		}

		pending = append(pending, hit)
		pendingIndices = append(pendingIndices, index)
	}

	if len(pending) == 0 {
		return covered, nil
	}

	verified, err := client.milvusCoverage.CoveredHits(ctx, collectionID, pending)
	if err != nil {
		slog.WarnContext(
			ctx,
			"daemon.search_conversations.milvus_coverage_failed",
			"concern",
			"conversation.search",
			"component",
			"daemon",
			"code",
			sourceRPCCode(err).String(),
		)

		return nil, fmt.Errorf("read Milvus passage coverage: %w", err)
	}

	if len(verified) != len(pending) {
		return nil, errCoverageCountMismatch
	}

	for index, value := range verified {
		client.checked[coverageKey(pending[index])] = value
		covered[pendingIndices[index]] = value
	}

	return covered, nil
}

func coverageKey(hit semsearch.SemHit) passageCoverageKey {
	return passageCoverageKey{
		conversationID: hit.ConversationID,
		relativePath:   hit.RelativePath,
		loadRules:      hit.LoadRules,
		content:        hit.Content,
		splitPart:      hit.SplitPart,
	}
}
