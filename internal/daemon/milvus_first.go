package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"google.golang.org/grpc/codes"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
)

type milvusFirstSearchSource struct {
	index    conversationSearchIndex
	settings config.ConversationSemanticConfig
	client   func(config.ConversationSemanticBackend) (conversationSemanticSearchClient, bool)
}

type searchCoverage interface {
	CoveredHits(ctx context.Context, collectionID string, hits []semsearch.SemHit) ([]bool, error)
}

type searchCollectionExistence interface {
	SearchCollectionExists(ctx context.Context, collectionID string) (bool, error)
}

const missingMilvusEmbeddings = "missing_milvus_embeddings"

var errSearchCoverageUnsupported = errors.New(
	"search client does not support Milvus embedding coverage",
)

type searchCoverageError struct{ cause error }

func (err searchCoverageError) Error() string { return "check Milvus embedding coverage" }
func (err searchCoverageError) Unwrap() error { return err.cause }

func (source *milvusFirstSearchSource) SearchConversations(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
) (conversation.SearchConversationsResult, error) {
	contextErr := ctx.Err()
	if contextErr != nil {
		slog.WarnContext(
			ctx,
			"daemon.search_conversations.interrupted",
			"concern",
			"conversation.search",
			"component",
			"daemon",
			"err",
			contextErr,
		)

		return conversation.SearchConversationsResult{}, fmt.Errorf(
			"search interrupted: %w",
			contextErr,
		)
	}

	if !source.settings.AnswersSearch() {
		return conversation.SearchConversationsResult{}, disabledConversationSearchSourceError(nil)
	}

	if options.Cursor != "" {
		return conversation.SearchConversationsResult{}, refusedConversationSearchSourceError(
			errSemanticSourceCursorUnsupported,
		)
	}

	limit, offset, target, err := semanticSearchPageBounds(options)
	if err != nil {
		return conversation.SearchConversationsResult{}, err
	}

	// Read one extra result when possible to distinguish a full final page.
	if target < maxInt32Value {
		target++
	}

	milvus, _ := source.client(config.ConversationSemanticBackendMilvus)
	if milvus == nil {
		return source.localFallback(ctx, options, string(conversationSearchSourceUnavailable))
	}

	if probe, ok := milvus.(searchCollectionExistence); ok {
		exists, err := probe.SearchCollectionExists(ctx, source.settings.CollectionID)
		if err != nil {
			failure := engineSearchCallError(ctx, err)
			if code, eligible := milvusFailureAllowsFallback(ctx, failure); eligible {
				return source.localFallback(ctx, options, string(code))
			}

			return conversation.SearchConversationsResult{}, failure
		}

		if !exists {
			return source.localFallback(ctx, options, missingMilvusEmbeddings)
		}
	}

	primary, err := engineSearchMatchesAt(
		ctx,
		source.index,
		milvus,
		source.settings.CollectionID,
		options,
		engineSearchBounds{limit: target, offset: 0, searchLimit: target, complete: true},
	)
	if err != nil {
		if code, eligible := milvusFailureAllowsFallback(ctx, err); eligible {
			return source.localFallback(ctx, options, string(code))
		}

		return conversation.SearchConversationsResult{}, err
	}

	markSearchBackend(primary.matches, conversation.SearchBackendMilvus)
	if len(primary.matches) >= target {
		return source.combine(ctx, options, primary, emptyEngineSearchPage(), limit, offset), nil
	}

	return source.supplementLocal(ctx, options, milvus, primary, limit, offset, target)
}

func (source *milvusFirstSearchSource) supplementLocal(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
	milvus conversationSemanticSearchClient,
	primary engineSearchPage,
	limit int,
	offset int,
	target int,
) (conversation.SearchConversationsResult, error) {
	milvusCoverage, ok := milvus.(searchCoverage)
	if !ok {
		return conversation.SearchConversationsResult{}, failedConversationSearchSourceError(
			errSearchCoverageUnsupported,
		)
	}

	local, _ := source.client(config.ConversationSemanticBackendLocal)
	if local == nil {
		return source.localSupplementFailed(
			ctx,
			options,
			primary,
			limit,
			offset,
			unavailableConversationSearchSourceError(nil),
		)
	}

	filtered := &uncoveredLocalSearchClient{
		conversationSemanticSearchClient: local,
		milvusCoverage:                   milvusCoverage,
		primary:                          primary.matches,
		checked:                          nil,
	}
	needed := target - len(primary.matches)
	secondary, err := engineSearchMatchesAt(
		ctx,
		source.index,
		filtered,
		source.settings.CollectionID,
		options,
		engineSearchBounds{limit: needed, offset: 0, searchLimit: needed, complete: true},
	)
	if err != nil {
		_, coverageFailure := errors.AsType[searchCoverageError](err)
		if code, eligible := milvusFailureAllowsFallback(ctx, err); eligible && coverageFailure {
			fallback, fallbackErr := source.localFallback(ctx, options, string(code))
			if fallbackErr == nil {
				return fallback, nil
			}

			err = fallbackErr
		}

		return source.localSupplementFailed(ctx, options, primary, limit, offset, err)
	}

	markSearchBackend(secondary.matches, conversation.SearchBackendLocal)
	if len(secondary.matches) > 0 {
		logLocalFallback(ctx, missingMilvusEmbeddings)
	}

	return source.combine(ctx, options, primary, secondary, limit, offset), nil
}

func (source *milvusFirstSearchSource) localSupplementFailed(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
	primary engineSearchPage,
	limit int,
	offset int,
	err error,
) (conversation.SearchConversationsResult, error) {
	if code, eligible := milvusFailureAllowsFallback(ctx, err); eligible &&
		len(primary.matches) > offset {
		slog.WarnContext(
			ctx,
			"daemon.search_conversations.local_supplement_failed",
			"concern",
			"conversation.search",
			"component",
			"daemon",
			"failure_code",
			string(code),
		)

		return source.combine(ctx, options, primary, emptyEngineSearchPage(), limit, offset), nil
	}

	return conversation.SearchConversationsResult{}, err
}

func (source *milvusFirstSearchSource) localFallback(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
	reason string,
) (conversation.SearchConversationsResult, error) {
	logLocalFallback(ctx, reason)
	local, _ := source.client(config.ConversationSemanticBackendLocal)
	if local == nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(
			nil,
		)
	}

	return semanticSearchResult(
		ctx,
		source.index,
		local,
		source.settings.CollectionID,
		conversation.SearchSourceLocal,
		options,
	)
}

func (source *milvusFirstSearchSource) combine(
	ctx context.Context,
	options conversation.SearchConversationsOptions,
	primary engineSearchPage,
	secondary engineSearchPage,
	limit int,
	offset int,
) conversation.SearchConversationsResult {
	matches := slices.Concat(primary.matches, secondary.matches)
	start := min(offset, len(matches))
	end := min(start+limit, len(matches))
	page := engineSearchPage{
		matches:   matches[start:end],
		ranked:    primary.ranked + secondary.ranked,
		withheld:  primary.withheld + secondary.withheld,
		truncated: false,
		short:     primary.short || secondary.short,
	}
	resultSource := conversation.SearchSourceSemantic
	if start < end && start >= len(primary.matches) {
		resultSource = conversation.SearchSourceLocal
	} else if end > len(primary.matches) {
		resultSource = conversation.SearchSourceMixed
	}

	result := searchResultPage(ctx, source.index, options, resultSource, page, limit, offset)
	result.HasMore = start < end && (end < len(matches) || page.short)

	return result
}

func logLocalFallback(ctx context.Context, reason string) {
	slog.InfoContext(
		ctx,
		"daemon.search_conversations.local_fallback",
		"concern",
		"conversation.search",
		"component",
		"daemon",
		"reason",
		reason,
	)
}

func milvusFailureAllowsFallback(
	ctx context.Context,
	err error,
) (conversationSearchFailureCode, bool) {
	if searchCancellationCode(ctx, err) != codes.OK {
		return "", false
	}

	failure, typed := errors.AsType[conversationSearchSourceError](err)
	if !typed {
		return "", false
	}

	code := sourceRPCCode(err)
	if code == codes.InvalidArgument || code == codes.OutOfRange {
		return failure.code, false
	}

	if code == codes.Internal || code == codes.Aborted || code == codes.ResourceExhausted {
		return failure.code, true
	}

	return failure.code, failure.code == conversationSearchSourceUnavailable ||
		failure.code == conversationSearchSourceFailed
}

func searchCancellationCode(ctx context.Context, err error) codes.Code {
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) ||
		sourceRPCCode(err) == codes.Canceled {
		return codes.Canceled
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return codes.DeadlineExceeded
	}

	return codes.OK
}
