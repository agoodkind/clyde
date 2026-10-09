package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
)

type exactRankingSearchClient interface {
	RanksEveryAllowedPassage() bool
}

type conversationSnapshotIndex interface {
	ListPage(ctx context.Context, options conversation.ListOptions) (conversation.ListResult, error)
}

func localSearchDispatch(idx conversationSearchIndex, semantic conversationSemanticSearchClient) (conversationSnapshotIndex, bool) {
	exact, ranksExactly := semantic.(exactRankingSearchClient)
	if !ranksExactly || !exact.RanksEveryAllowedPassage() {
		return nil, false
	}
	snapshots, listsSnapshots := idx.(conversationSnapshotIndex)
	return snapshots, listsSnapshots
}

func localSearchAllowedRecords(
	ctx context.Context,
	snapshots conversationSnapshotIndex,
	options conversation.SearchConversationsOptions,
) (map[string]conversation.Record, error) {
	snapshot, err := snapshots.ListPage(ctx, conversation.ListOptions{
		Limit:           0,
		Offset:          0,
		Provider:        options.Provider,
		WorkspaceRoot:   options.WorkspaceRoot,
		Query:           "",
		IncludeArchived: options.IncludeArchived,
		All:             true,
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.local_scope_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil, failedConversationSearchSourceError(fmt.Errorf("resolve allowed conversations: %w", err))
	}
	allowed := make(map[string]conversation.Record, len(snapshot.Records))
	for _, record := range snapshot.Records {
		if options.ConversationID != "" && record.ID != options.ConversationID {
			continue
		}
		allowed[record.ID] = record
	}
	return allowed, nil
}

func localSearchMatches(
	ctx context.Context,
	snapshots conversationSnapshotIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	options conversation.SearchConversationsOptions,
) (engineSearchPage, error) {
	_, offset, searchLimit, err := semanticSearchPageBounds(options)
	if err != nil {
		return emptyEngineSearchPage(), err
	}
	allowed, err := localSearchAllowedRecords(ctx, snapshots, options)
	if err != nil {
		return emptyEngineSearchPage(), err
	}
	if len(allowed) == 0 {
		return emptyEngineSearchPage(), nil
	}
	conversationIDs := make([]string, 0, len(allowed))
	for conversationID := range allowed {
		conversationIDs = append(conversationIDs, conversationID)
	}
	filter := semsearch.SearchFilter{
		Providers:            nil,
		WorkspaceRoots:       nil,
		Roles:                options.Roles,
		FromUnix:             options.FromUnix,
		UntilUnix:            options.UntilUnix,
		ConversationIDs:      conversationIDs,
		ParentConversationID: "",
		MinScore:             options.MinScore,
		MessageIndexFrom:     0,
		MessageIndexUntil:    0,
	}
	hits, err := semantic.SearchConversations(
		ctx,
		collectionID,
		options.Query,
		int32FromInt(searchLimit),
		filter,
		int32FromInt(options.PerConversationLimit),
	)
	if err != nil {
		return emptyEngineSearchPage(), engineSearchCallError(ctx, err)
	}
	window := hits[min(offset, len(hits)):]
	matches := make([]conversation.SearchMatch, 0, len(window))
	for _, hit := range window {
		record, found := allowed[hit.ConversationID]
		if !found {
			return emptyEngineSearchPage(), failedConversationSearchSourceError(
				fmt.Errorf("local search returned conversation %q outside the allowed set", hit.ConversationID),
			)
		}
		matches = append(matches, conversation.SearchMatch{
			Record:        record,
			MessageIndex:  int(hit.MessageIndex),
			Role:          hit.Role,
			Timestamp:     time.Unix(hit.TimestampUnix, 0),
			Snippet:       conversation.Snippet(hit.Content),
			Score:         hit.Score,
			ContextWindow: conversation.Excerpt(hit.Content),
			LoadRules:     hit.LoadRules,
			ContextState:  conversation.SearchContextStateExcerptOnly,
		})
	}
	return engineSearchPage{
		matches:   matches,
		ranked:    len(hits),
		withheld:  0,
		truncated: len(hits) >= searchLimit,
		short:     false,
	}, nil
}
