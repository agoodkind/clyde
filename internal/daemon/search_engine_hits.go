package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
)

const (
	// searchOverfetchAttempts bounds how many times one page is re-queried at a
	// larger engine limit after hits that no longer resolve consumed ranked slots.
	// Hidden subagent rows stay in the vector store under the retain reconcile
	// mode and stay rankable, so without headroom every one of them costs the
	// caller a result.
	searchOverfetchAttempts = 3
	// searchOverfetchFactor multiplies the engine limit on each retry.
	searchOverfetchFactor = 4
	// maxEngineOverfetchLimit caps the retry so one query can never ask the engine
	// for an unbounded ranked set.
	maxEngineOverfetchLimit = 2000
)

// engineSearchPage is one resolved page of engine hits.
type engineSearchPage struct {
	// matches are the hits that resolved to a served record, after paging.
	matches []conversation.SearchMatch
	// ranked is how many engine hits this page consumed.
	ranked int
	// withheld is how many of those hits were dropped because their conversation
	// is hidden, no longer indexed, or archived out of the request.
	withheld int
	// truncated reports that the engine filled the requested limit, so more
	// ranked hits exist beyond the ones this page saw.
	truncated bool
	// short reports a page that could not be filled while ranked hits were being
	// withheld and the engine still had more, which is the one case where the
	// caller must not be told the results are complete.
	short bool
}

// emptyEngineSearchPage is the zero page. A short-circuited query returns it
// with a nil error, which the caller reads as no usable hits; a failed query
// returns it beside the typed error the caller reads instead.
func emptyEngineSearchPage() engineSearchPage {
	return engineSearchPage{
		matches:   []conversation.SearchMatch{},
		ranked:    0,
		withheld:  0,
		truncated: false,
		short:     false,
	}
}

// emptySearchFilter is the filter a scope that resolved to nothing returns. It
// is never sent to the engine.
func emptySearchFilter() semsearch.SearchFilter {
	return semsearch.SearchFilter{
		Providers:            nil,
		WorkspaceRoots:       nil,
		Roles:                nil,
		FromUnix:             0,
		UntilUnix:            0,
		ConversationIDs:      nil,
		ParentConversationID: "",
		MinScore:             0,
		MessageIndexFrom:     0,
		MessageIndexUntil:    0,
	}
}

// engineSearchMatches resolves engine hits to cached records, skipping hits with
// no record or that fail the request provider, workspace, or archived filter,
// and returns the bounded page. A page that comes up short because hidden
// conversations occupied ranked slots is re-queried at a larger engine limit, a
// bounded number of times, so hiding a conversation cannot silently shrink a
// result set. The engine wire filter carries no exclusion field, so the headroom
// has to be taken clyde-side.
//
// Every engine failure stays a typed conversationSearchSourceError; only a
// successful search may return an empty page.
func engineSearchMatches(
	ctx context.Context,
	idx conversationSearchIndex,
	semantic conversationSemanticSearchClient,
	collectionID string,
	options conversation.SearchConversationsOptions,
) (engineSearchPage, error) {
	limit, offset, searchLimit, err := semanticSearchPageBounds(options)
	if err != nil {
		return emptyEngineSearchPage(), err
	}
	filter, scoped, err := engineSearchFilter(ctx, idx, options)
	if err != nil {
		return emptyEngineSearchPage(), err
	}
	if !scoped {
		// The workspace matched no conversations, so the result is empty. An empty
		// id set would otherwise read as "no scope" and match the whole corpus, so
		// short-circuit instead of calling the engine.
		return emptyEngineSearchPage(), nil
	}

	engineLimit := searchLimit
	var page engineSearchPage
	for attempt := 1; ; attempt++ {
		hits, searchErr := semantic.SearchConversations(
			ctx,
			collectionID,
			options.Query,
			int32FromInt(engineLimit),
			filter,
			int32FromInt(options.PerConversationLimit),
		)
		if searchErr != nil {
			return emptyEngineSearchPage(), engineSearchCallError(ctx, searchErr)
		}
		page = resolveEngineHits(idx, hits, limit, offset, options.IncludeArchived, engineLimit)
		nextLimit := nextEngineOverfetchLimit(engineLimit)
		if !page.needsMoreHeadroom(limit) || attempt >= searchOverfetchAttempts || nextLimit == engineLimit {
			break
		}
		engineLimit = nextLimit
	}
	if page.needsMoreHeadroom(limit) {
		page.short = true
		slog.WarnContext(ctx, "daemon.search_conversations.page_short_after_overfetch", "concern", "process.daemon.lifecycle", "component", "daemon",
			"limit", limit,
			"returned", len(page.matches),
			"ranked", page.ranked,
			"withheld", page.withheld,
			"engine_limit", engineLimit,
		)
	}
	return page, nil
}

// engineSearchCallError classifies one failed engine call into the typed source
// error the control server renders, so no engine failure can be mistaken for an
// empty result set.
func engineSearchCallError(ctx context.Context, err error) error {
	slog.WarnContext(ctx, "daemon.search_conversations.source_call_failed",
		"concern", "process.daemon.lifecycle",
		"component", "daemon",
		"source", conversation.SearchSourceSemantic.String(),
		"err", err,
	)
	if isConversationSearchSourceUnavailable(err) {
		return unavailableConversationSearchSourceError(err)
	}
	if sourceRPCCode(err) != codes.Unknown {
		return refusedConversationSearchSourceError(err)
	}
	return failedConversationSearchSourceError(err)
}

// engineSearchFilter builds the engine-side filter for a request. The second
// result is false when a workspace scope resolved to no conversations, which
// means the answer is empty without asking the engine at all.
func engineSearchFilter(
	ctx context.Context,
	idx conversationSearchIndex,
	options conversation.SearchConversationsOptions,
) (semsearch.SearchFilter, bool, error) {
	provider := options.Provider
	var providers []string
	if provider.Valid() {
		providers = []string{provider.String()}
	}
	var conversationIDs []string
	switch {
	case options.ConversationID != "":
		// A conversation_id scopes the engine to that one conversation, the
		// within-search behavior. Provider and workspace scope are irrelevant
		// then but still safe to pass; the id set is the tightest filter.
		conversationIDs = []string{options.ConversationID}
	case options.WorkspaceRoot != "":
		resolved, err := workspaceConversationIDs(ctx, idx, options.WorkspaceRoot)
		if err != nil {
			return emptySearchFilter(), false, err
		}
		if len(resolved) == 0 {
			return emptySearchFilter(), false, nil
		}
		conversationIDs = resolved
	}
	return semsearch.SearchFilter{
		Providers:            providers,
		WorkspaceRoots:       nil,
		Roles:                options.Roles,
		FromUnix:             options.FromUnix,
		UntilUnix:            options.UntilUnix,
		ConversationIDs:      conversationIDs,
		ParentConversationID: "",
		MinScore:             options.MinScore,
		MessageIndexFrom:     0,
		MessageIndexUntil:    0,
	}, true, nil
}

// needsMoreHeadroom reports a page that fell short of the requested limit
// because ranked hits were withheld while the engine still had more to give.
// A page short for any other reason is the honest end of the result set.
func (page engineSearchPage) needsMoreHeadroom(limit int) bool {
	if limit <= 0 || len(page.matches) >= limit {
		return false
	}
	return page.withheld > 0 && page.truncated
}

// nextEngineOverfetchLimit grows the engine limit for a retry, stopping at the
// cap. It returns the current limit unchanged once there is no room left to
// grow, which ends the retry loop.
func nextEngineOverfetchLimit(engineLimit int) int {
	if engineLimit >= maxEngineOverfetchLimit {
		return engineLimit
	}
	grown := engineLimit * searchOverfetchFactor
	if grown > maxEngineOverfetchLimit || grown < engineLimit {
		return maxEngineOverfetchLimit
	}
	return grown
}

// resolveEngineHits hydrates ranked engine hits into matches, applying the
// clyde-side filters and the page offset, and counts what it dropped so the
// caller can tell a genuinely exhausted result set from one the filters thinned.
func resolveEngineHits(
	idx conversationSearchIndex,
	hits []semsearch.SemHit,
	limit int,
	offset int,
	includeArchived bool,
	engineLimit int,
) engineSearchPage {
	matches := make([]conversation.SearchMatch, 0, len(hits))
	withheld := 0
	consumed := 0
	seenMatches := 0
	for _, hit := range hits {
		consumed++
		record, ok := idx.RecordByID(hit.ConversationID)
		if !ok {
			// The conversation is hidden by the subagent setting or is no longer in
			// the index, while its chunks stay ranked in the store.
			withheld++
			continue
		}
		// provider is filtered natively by the engine; workspace is filtered via
		// the resolved conversation-id set above, not the native workspace column,
		// which is null on rows indexed before it existed. archived is a clyde
		// record flag not stored on the engine rows, so it stays a clyde-side
		// check. Archived conversations are rare, so dropping one from the top-K
		// has negligible effect on recall.
		if record.Archived && !includeArchived {
			withheld++
			continue
		}
		if seenMatches < offset {
			seenMatches++
			continue
		}
		matches = append(matches, conversation.SearchMatch{
			Record:       record,
			MessageIndex: int(hit.MessageIndex),
			Role:         hit.Role,
			Timestamp:    time.Unix(hit.TimestampUnix, 0),
			Snippet:      conversation.Snippet(hit.Content),
			Score:        hit.Score,
			// The excerpt is the matched passage the engine already returned,
			// byte-bounded so the ranked list stays small. The full surrounding
			// window is a separate windowed read; search never inlines it.
			ContextWindow: conversation.Excerpt(hit.Content),
			LoadRules:     hit.LoadRules,
		})
		seenMatches++
		if limit > 0 && len(matches) >= limit {
			break
		}
	}
	return engineSearchPage{
		matches:   matches,
		ranked:    consumed,
		withheld:  withheld,
		truncated: len(hits) >= engineLimit,
		short:     false,
	}
}

// workspaceConversationIDs resolves a workspace filter to the conversation-id
// set under it (prefix match, so a project root includes its subdirectories),
// regardless of provider or archived state, which are handled separately. The
// engine filters natively on the conversation_id column and batches a large set.
func workspaceConversationIDs(ctx context.Context, idx conversationSearchIndex, workspace string) ([]string, error) {
	var anyProvider conversation.Provider
	conversationIDs, err := idx.ConversationIDsMatching(ctx, anyProvider, workspace, true)
	if err != nil {
		slog.WarnContext(ctx, "daemon.search_conversations.scope_failed", "concern", "process.daemon.lifecycle", "component", "daemon",
			"err", err,
		)
		return nil, failedConversationSearchSourceError(
			fmt.Errorf("resolve workspace conversation ids for %q: %w", workspace, err),
		)
	}
	return conversationIDs, nil
}
