package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/lm-semantic-search/library"
	"google.golang.org/grpc/codes"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
)

// embeddedConversationSearchSource ranks committed occurrences independently
// of the raw index. Missing artifacts retain their stored excerpts.
type embeddedConversationSearchSource struct {
	library  *library.Library
	semantic config.ConversationSemanticConfig
	gate     *embeddedReconcileGate
	index    *conversation.Index
	outbox   *conversationSemanticOutbox
}

func (source *embeddedConversationSearchSource) SearchConversations(ctx context.Context, options conversation.SearchConversationsOptions) (conversation.SearchConversationsResult, error) {
	if source != nil && !source.semantic.SearchEnabled {
		return conversation.SearchConversationsResult{}, disabledConversationSearchSourceError(nil)
	}
	if source == nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(nil)
	}
	if source.gate != nil {
		return source.gate.search(ctx, source.semantic, options, source.index)
	}
	if source.library == nil {
		return conversation.SearchConversationsResult{}, unavailableConversationSearchSourceError(nil)
	}
	filter, err := embeddedConversationFilter(source.semantic, options)
	if err != nil {
		return conversation.SearchConversationsResult{}, refusedConversationSearchSourceError(err)
	}
	limit := normalizedSearchLimit(options.Limit)
	offset := normalizedPagingOffset(options.Offset)
	request := library.SearchRequest{
		Namespace: source.semantic.CollectionID, Query: strings.ReplaceAll(options.Query, "\x00", " "),
		Filter: filter, MinScore: options.MinScore, PageSize: limit,
		Cursor: options.Cursor, PerGroupLimit: options.PerConversationLimit,
	}
	if options.PerConversationLimit > 0 {
		request.GroupBy = embeddedScalarConversationID
	}
	remaining := offset
	if request.Cursor != "" {
		// Cursor ordinals already select the next occurrence. Offset remains
		// response accounting rather than a second skip inside the snapshot.
		remaining = 0
	}
	for remaining > 0 {
		request.PageSize = min(limit, remaining)
		page, searchErr := source.library.Search(ctx, request)
		if searchErr != nil {
			return conversation.SearchConversationsResult{}, embeddedSearchCallError(ctx, searchErr)
		}
		remaining -= len(page.Hits)
		if !page.HasMore {
			return embeddedSearchResult(nil, limit, offset, library.SearchPage{}), nil
		}
		if len(page.Hits) == 0 || page.NextCursor == "" {
			return conversation.SearchConversationsResult{}, failedConversationSearchSourceError(errors.New("embedded search returned an invalid continuation"))
		}
		request.Cursor = page.NextCursor
	}
	request.PageSize = limit
	page, err := source.library.Search(ctx, request)
	if err != nil {
		return conversation.SearchConversationsResult{}, embeddedSearchCallError(ctx, err)
	}
	matches := make([]conversation.SearchMatch, 0, len(page.Hits))
	for _, hit := range page.Hits {
		match, hydrateErr := embeddedSearchMatch(hit)
		if hydrateErr != nil {
			return conversation.SearchConversationsResult{}, embeddedSearchCallError(ctx, hydrateErr)
		}
		match, contextErr := source.verifyContext(ctx, hit, match, options)
		if contextErr != nil {
			return conversation.SearchConversationsResult{}, embeddedSearchCallError(ctx, contextErr)
		}
		matches = append(matches, match)
	}
	return embeddedSearchResult(matches, limit, offset, page), nil
}

func embeddedSearchResult(matches []conversation.SearchMatch, limit, offset int, page library.SearchPage) conversation.SearchConversationsResult {
	if matches == nil {
		matches = []conversation.SearchMatch{}
	}
	return conversation.SearchConversationsResult{
		Matches: matches, ConversationsScanned: len(matches), ReturnedCount: len(matches),
		Limit: limit, Offset: offset, NextOffset: offset + len(matches),
		HasMore: page.HasMore, NextCursor: page.NextCursor, Source: conversation.SearchSourceSemantic,
		Facets:           conversation.ComputeFacets(matches, searchFacetTopN),
		Freshness:        conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting: appendReturnedStage(nil, len(matches)),
	}
}

func embeddedSearchCallError(ctx context.Context, err error) conversationSearchSourceError {
	slog.WarnContext(ctx, "daemon.conversation_embedded_search.failed", "component", "daemon", "concern", "conversation.semantic", "err", err)
	failure := failedConversationSearchSourceError(err)
	switch {
	case errors.Is(err, library.ErrInvalidRequest), errors.Is(err, library.ErrCursorMismatch):
		failure.code, failure.rpcCode = conversationSearchSourceRefused, codes.InvalidArgument
	case errors.Is(err, library.ErrCursorExpired), errors.Is(err, library.ErrStoreMismatch):
		failure.code, failure.rpcCode = conversationSearchSourceRefused, codes.FailedPrecondition
	case errors.Is(err, library.ErrResourceLimit):
		failure.code, failure.rpcCode = conversationSearchSourceRefused, codes.ResourceExhausted
	case errors.Is(err, library.ErrDeadline), errors.Is(err, context.DeadlineExceeded):
		failure.rpcCode = codes.DeadlineExceeded
	}
	return failure
}

func embeddedSearchMatch(hit library.SearchHit) (conversation.SearchMatch, error) {
	conversationID, err := embeddedHitScalar(hit, embeddedScalarConversationID, library.String, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	if conversationID.String == "" || conversationID.String != hit.ID.OwnerID {
		return conversation.SearchMatch{}, errors.New("embedded occurrence owner does not match conversation metadata")
	}
	provider, err := embeddedHitScalar(hit, embeddedScalarProvider, library.String, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	providerID, known := providerid.Parse(provider.String)
	if !known || !providerID.Valid() {
		return conversation.SearchMatch{}, errors.New("embedded occurrence has an invalid provider")
	}
	workspace, err := embeddedHitScalar(hit, embeddedScalarWorkspaceRoot, library.String, true)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	archived, err := embeddedHitScalar(hit, embeddedScalarArchived, library.Bool, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	subagent, err := embeddedHitScalar(hit, embeddedScalarSubagent, library.Bool, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	role, err := embeddedHitScalar(hit, embeddedScalarRole, library.String, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	timestamp, err := embeddedHitScalar(hit, embeddedScalarTimestampUnix, library.Int64, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	messageIndex, err := embeddedHitScalar(hit, embeddedScalarMessageIndex, library.Int64, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	if messageIndex.Int64 < 0 || int64(int(messageIndex.Int64)) != messageIndex.Int64 {
		return conversation.SearchMatch{}, errors.New("embedded occurrence has an invalid message index")
	}
	loadRules, err := embeddedHitScalar(hit, embeddedScalarLoadRules, library.String, false)
	if err != nil {
		return conversation.SearchMatch{}, err
	}
	record := conversation.Record{
		ID: conversationID.String, Provider: providerID, NativeID: "", Selector: "", Lineage: nil,
		Origin: conversation.OriginUnspecified, Title: "", TitleUncertain: false, WorkspaceRoot: workspace.String,
		ArtifactPath: "", ArtifactKind: "", Model: "", CreatedAt: time.Time{}, UpdatedAt: time.Time{},
		SizeBytes: 0, Archived: archived.Bool, LatestRequestID: "",
	}
	if subagent.Bool {
		record.Origin = conversation.OriginSubagent
	}
	return conversation.SearchMatch{
		Record: record, MessageIndex: int(messageIndex.Int64), Role: role.String,
		Timestamp: time.Unix(timestamp.Int64, 0), Score: hit.Score,
		Snippet: conversation.Snippet(hit.SourceText), ContextWindow: conversation.Excerpt(hit.SourceText),
		LoadRules: loadRules.String, ContextState: conversation.SearchContextStateUnavailable,
	}, nil
}

func embeddedHitScalar(hit library.SearchHit, column string, scalarType library.ScalarType, nullable bool) (library.ScalarValue, error) {
	value, exists := hit.Scalars[column]
	if !exists || value.Type != scalarType || (value.Null && !nullable) {
		return library.ScalarValue{}, fmt.Errorf("embedded occurrence has invalid %s metadata", column)
	}
	return value, nil
}
