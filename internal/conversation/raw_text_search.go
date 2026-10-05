package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// ErrQueryRequired reports a search request with an empty query.
var ErrQueryRequired = errors.New("query is required")

// SearchRawText scans transcript text in conversation list order and returns
// the first message in each conversation that contains every query term. The
// match is case-insensitive. Each match has a zero score.
func (idx *Index) SearchRawText(ctx context.Context, options SearchConversationsOptions) (SearchConversationsResult, error) {
	options = normalizeRawTextOptions(options)
	terms := queryTerms(options.Query)
	if len(terms) == 0 {
		return SearchConversationsResult{}, ErrQueryRequired
	}
	candidates, err := idx.ListPage(ctx, ListOptions{
		Limit:           0,
		Offset:          0,
		Provider:        options.Provider,
		WorkspaceRoot:   options.WorkspaceRoot,
		Query:           "",
		IncludeArchived: options.IncludeArchived,
		All:             true,
	})
	if err != nil {
		return SearchConversationsResult{}, err
	}
	result := SearchConversationsResult{
		Matches:              nil,
		ConversationsScanned: 0,
		ReturnedCount:        0,
		Limit:                options.Limit,
		Offset:               options.Offset,
		NextOffset:           options.Offset,
		HasMore:              false,
		NextCursor:           "",
		Source:               SearchSourceRawText,
		Facets:               SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
		Freshness:            SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting:     nil,
	}
	skipped := 0
	for _, record := range candidates.Records {
		if options.ConversationID != "" && record.ID != options.ConversationID {
			continue
		}
		if err := ctx.Err(); err != nil {
			return SearchConversationsResult{}, fmt.Errorf("search raw text: %w", err)
		}
		result.ConversationsScanned++
		match, found, matchErr := idx.firstRawTextMatch(record, terms, options)
		if matchErr != nil {
			slog.WarnContext(ctx, "conversation.raw_text_search.read_failed",
				"concern", "conversation.search",
				"conversation_id", record.ID,
				"err", matchErr,
			)
			return SearchConversationsResult{}, fmt.Errorf("search raw text of conversation %s: %w", record.ID, matchErr)
		}
		if !found {
			continue
		}
		if skipped < options.Offset {
			skipped++
			continue
		}
		if len(result.Matches) == options.Limit {
			result.HasMore = true
			break
		}
		result.Matches = append(result.Matches, match)
	}
	result.ReturnedCount = len(result.Matches)
	result.NextOffset = options.Offset + result.ReturnedCount
	result.Facets = ComputeFacets(result.Matches, 0)
	return result, nil
}

func (idx *Index) firstRawTextMatch(record Record, terms []string, options SearchConversationsOptions) (SearchMatch, bool, error) {
	stream, err := idx.resolveStream(record, LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
		IncludeInjected:       false,
		HarnessTally:          nil,
	})
	if err != nil {
		return emptySearchMatch(), false, err
	}
	messageIndex := 0
	for message, streamErr := range stream {
		if streamErr != nil {
			return emptySearchMatch(), false, streamErr
		}
		if messageMatchesRowFilters(message, options.Roles, options.FromUnix, options.UntilUnix) {
			indexText := transcript.RenderMessageIndexText(message)
			if textContainsTerms(indexText, terms) {
				return SearchMatch{
					Record:        record,
					MessageIndex:  messageIndex,
					Role:          message.Role,
					Timestamp:     message.Timestamp,
					Snippet:       snippet(indexText),
					Score:         0,
					ContextWindow: Excerpt(indexText),
					LoadRules:     "",
					ContextState:  SearchContextStateExcerptOnly,
				}, true, nil
			}
		}
		messageIndex++
	}
	return emptySearchMatch(), false, nil
}

func emptySearchMatch() SearchMatch {
	return SearchMatch{
		Record:        emptyRecord(),
		MessageIndex:  0,
		Role:          "",
		Timestamp:     time.Time{},
		Snippet:       "",
		Score:         0,
		ContextWindow: "",
		LoadRules:     "",
		ContextState:  SearchContextStateUnspecified,
	}
}

// messageMatchesRowFilters applies the role and time filters. The lower time
// bound is inclusive and the upper bound is exclusive.
func messageMatchesRowFilters(message transcript.Message, roles []string, fromUnix int64, untilUnix int64) bool {
	if len(roles) > 0 {
		matched := false
		for _, role := range roles {
			if strings.EqualFold(role, message.Role) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	timestamp := message.Timestamp.Unix()
	if fromUnix > 0 && timestamp < fromUnix {
		return false
	}
	if untilUnix > 0 && timestamp >= untilUnix {
		return false
	}
	return true
}

// textContainsTerms lowercases text only when it contains an ASCII uppercase
// letter or a non-ASCII byte. Tool output can be several megabytes.
func textContainsTerms(text string, terms []string) bool {
	if needsLowering(text) {
		text = strings.ToLower(text)
	}
	for _, term := range terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func needsLowering(text string) bool {
	for i := range len(text) {
		character := text[i]
		if (character >= 'A' && character <= 'Z') || character >= 0x80 {
			return true
		}
	}
	return false
}

func normalizeRawTextOptions(options SearchConversationsOptions) SearchConversationsOptions {
	options.Query = strings.TrimSpace(options.Query)
	options.WorkspaceRoot = cleanWorkspaceFilter(options.WorkspaceRoot)
	if options.Offset < 0 {
		options.Offset = 0
	}
	if options.Limit <= 0 {
		options.Limit = DefaultSearchLimit
	}
	if options.Limit > MaxSearchLimit {
		options.Limit = MaxSearchLimit
	}
	return options
}
