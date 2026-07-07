package conversation

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"goodkind.io/clyde/internal/transcript"
)

const (
	// DefaultListLimit keeps no-argument list calls small enough for MCP clients.
	DefaultListLimit = 20
	// MaxListLimit bounds each page even when a client requests a very large limit.
	MaxListLimit = 100
	// DefaultSearchLimit keeps cross-conversation discovery results compact.
	DefaultSearchLimit = 20
	// MaxSearchLimit bounds transcript discovery output.
	MaxSearchLimit = 50

	searchSnippetRunes = 240
)

// ListOptions filters and pages conversation metadata.
type ListOptions struct {
	Limit           int
	Offset          int
	Provider        Provider
	WorkspaceRoot   string
	Query           string
	IncludeArchived bool
	All             bool
}

// ListResult is one bounded page of conversation metadata.
type ListResult struct {
	Records       []Record
	TotalMatched  int
	ReturnedCount int
	Offset        int
	Limit         int
	NextOffset    int
	HasMore       bool
}

// SearchConversationsOptions filters a bounded transcript discovery pass.
type SearchConversationsOptions struct {
	Query           string
	Limit           int
	Offset          int
	Provider        Provider
	WorkspaceRoot   string
	IncludeArchived bool
	// Roles, FromUnix, UntilUnix, and MinScore narrow engine retrieval by row
	// attributes; PerConversationLimit caps hits per conversation. The live
	// literal fallback honors roles and time bounds and ignores the score
	// knobs, which only mean something for engine relevance.
	Roles                []string
	FromUnix             int64
	UntilUnix            int64
	MinScore             float64
	PerConversationLimit int
	// ConversationID scopes discovery to a single conversation, the within-search
	// behavior. Empty means corpus-wide discovery.
	ConversationID string
	// ContextWindow is the number of messages before and after each hit to render
	// inline on the match. Zero means the daemon's default small window.
	ContextWindow int
}

// SearchMatch is the first matching message found in one conversation during
// cross-conversation discovery.
type SearchMatch struct {
	Record       Record
	MessageIndex int
	Role         string
	Timestamp    time.Time
	Snippet      string
	// Score is the engine's retrieval relevance; zero on literal-scan matches.
	Score float64
	// ContextWindow is the rendered messages surrounding this hit; empty on
	// literal-scan matches and when the window render failed.
	ContextWindow string
}

// SearchConversationsResult is a bounded set of candidate conversations.
type SearchConversationsResult struct {
	Matches              []SearchMatch
	ConversationsScanned int
	ReturnedCount        int
	Limit                int
	Offset               int
	NextOffset           int
	HasMore              bool
	// Source names which engine produced the matches: the vector engine, the
	// literal fallback, or a cold index with the fallback disabled.
	Source SearchSource
	// Facets summarizes the match set by workspace, provider, and model.
	Facets SearchFacets
	// Freshness is the conversation-index sync state at query time.
	Freshness SearchFreshness
	// FilterAccounting is the ordered funnel of candidate counts per filter.
	FilterAccounting []FilterStage
}

// ListPage returns one filtered, bounded page from the cached index.
func (idx *Index) ListPage(ctx context.Context, options ListOptions) (ListResult, error) {
	records, err := idx.List(ctx)
	if err != nil {
		return ListResult{}, err
	}
	return FilterRecords(records, options), nil
}

// ConversationIDsMatching resolves record-level filters (provider, workspace,
// archived) into the conversation-id set that scopes engine retrieval, so a
// record-scoped search filters in the vector store instead of post-filtering
// engine hits.
func (idx *Index) ConversationIDsMatching(ctx context.Context, provider Provider, workspaceRoot string, includeArchived bool) ([]string, error) {
	result, err := idx.ListPage(ctx, ListOptions{
		Limit:           0,
		Offset:          0,
		Provider:        provider,
		WorkspaceRoot:   workspaceRoot,
		Query:           "",
		IncludeArchived: includeArchived,
		All:             true,
	})
	if err != nil {
		return nil, err
	}
	conversationIDs := make([]string, 0, len(result.Records))
	for _, record := range result.Records {
		conversationIDs = append(conversationIDs, record.ID)
	}
	return conversationIDs, nil
}

// FilterRecords applies list filters and pagination to a pre-sorted record slice.
func FilterRecords(records []Record, options ListOptions) ListResult {
	options = normalizeListOptions(options)
	filtered := make([]Record, 0, len(records))
	for _, record := range records {
		if recordMatchesListOptions(record, options) {
			filtered = append(filtered, record)
		}
	}

	totalMatched := len(filtered)
	start := min(options.Offset, totalMatched)

	end := totalMatched
	if !options.All {
		end = min(start+options.Limit, totalMatched)
	}

	page := cloneRecords(filtered[start:end])
	nextOffset := start + len(page)
	hasMore := nextOffset < totalMatched
	return ListResult{
		Records:       page,
		TotalMatched:  totalMatched,
		ReturnedCount: len(page),
		Offset:        start,
		Limit:         options.Limit,
		NextOffset:    nextOffset,
		HasMore:       hasMore,
	}
}

// SearchConversations finds candidate conversations by scanning transcript text
// until the bounded result set is full or every filtered conversation was read.
func (idx *Index) SearchConversations(ctx context.Context, options SearchConversationsOptions) (SearchConversationsResult, error) {
	options = normalizeSearchConversationsOptions(options)
	terms := queryTerms(options.Query)
	if len(terms) == 0 {
		return SearchConversationsResult{
			Matches:              nil,
			ConversationsScanned: 0,
			ReturnedCount:        0,
			Limit:                options.Limit,
			Offset:               options.Offset,
			NextOffset:           options.Offset,
			HasMore:              false,
			Source:               SearchSourceUnspecified,
			Facets:               SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
			Freshness:            SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
			FilterAccounting:     nil,
		}, errorsQueryRequired()
	}

	listOptions := ListOptions{
		Limit:           0,
		Offset:          0,
		Provider:        options.Provider,
		WorkspaceRoot:   options.WorkspaceRoot,
		Query:           "",
		IncludeArchived: options.IncludeArchived,
		All:             true,
	}
	candidates, err := idx.ListPage(ctx, listOptions)
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
		Source:               SearchSourceLiteral,
		Facets:               SearchFacets{Workspaces: nil, Providers: nil, Models: nil},
		Freshness:            SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		FilterAccounting:     nil,
	}
	seenMatches := 0
	for _, record := range candidates.Records {
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("search conversations canceled: %w", ctx.Err())
		default:
		}
		result.ConversationsScanned++
		match, ok, err := idx.firstTranscriptMatch(record, terms, options)
		if err != nil {
			slog.WarnContext(ctx, "conversation.search_conversations.match_failed", "concern", "conversation.search", "conversation_id", record.ID, "err", err)
			return result, fmt.Errorf("search conversation %s: %w", record.ID, err)
		}
		if !ok {
			continue
		}
		if seenMatches < options.Offset {
			seenMatches++
			continue
		}
		result.Matches = append(result.Matches, match)
		result.ReturnedCount = len(result.Matches)
		result.NextOffset = options.Offset + result.ReturnedCount
		seenMatches++
		if result.ReturnedCount >= options.Limit {
			result.HasMore = result.ConversationsScanned < len(candidates.Records)
			return result, nil
		}
	}
	return result, nil
}

func (idx *Index) firstTranscriptMatch(record Record, terms []string, options SearchConversationsOptions) (SearchMatch, bool, error) {
	stream, err := idx.resolveStream(record, LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	})
	if err != nil {
		return emptySearchMatch(), false, err
	}
	messageIndex := 0
	for message, streamErr := range stream {
		if streamErr != nil {
			return emptySearchMatch(), false, streamErr
		}
		if !messageMatchesRowFilters(message, options.Roles, options.FromUnix, options.UntilUnix) {
			messageIndex++
			continue
		}
		indexText := transcript.RenderMessageIndexText(message)
		if messageIndexTextMatchesTerms(indexText, terms) {
			return SearchMatch{
				Record:        record,
				MessageIndex:  messageIndex,
				Role:          message.Role,
				Timestamp:     message.Timestamp,
				Snippet:       snippet(indexText),
				Score:         0,
				ContextWindow: "",
			}, true, nil
		}
		messageIndex++
	}
	return emptySearchMatch(), false, nil
}

// messageMatchesRowFilters applies the role and time conditions shared by the
// literal scans, mirroring the engine-side filter semantics: inclusive from,
// exclusive until, case-insensitive roles, empty conditions match everything.
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

func normalizeListOptions(options ListOptions) ListOptions {
	if options.Offset < 0 {
		options.Offset = 0
	}
	options.WorkspaceRoot = cleanWorkspaceFilter(options.WorkspaceRoot)
	options.Query = strings.TrimSpace(options.Query)
	if options.All {
		options.Limit = 0
		return options
	}
	if options.Limit <= 0 {
		options.Limit = DefaultListLimit
	}
	if options.Limit > MaxListLimit {
		options.Limit = MaxListLimit
	}
	return options
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
	}
}

func normalizeSearchConversationsOptions(options SearchConversationsOptions) SearchConversationsOptions {
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

func recordMatchesListOptions(record Record, options ListOptions) bool {
	if !options.IncludeArchived && record.Archived {
		return false
	}
	if options.Provider.Valid() && record.Provider != options.Provider {
		return false
	}
	if options.WorkspaceRoot != "" {
		filterWorkspace := cleanWorkspaceFilter(options.WorkspaceRoot)
		recordWorkspace := cleanWorkspaceFilter(record.WorkspaceRoot)
		// Prefix match so a project-root filter includes its subdirectories: a
		// filter of ~/Sites/clyde-dev matches ~/Sites/clyde-dev and
		// ~/Sites/clyde-dev/clyde, not just the exact path.
		if recordWorkspace != filterWorkspace && !strings.HasPrefix(recordWorkspace, filterWorkspace+"/") {
			return false
		}
	}
	terms := queryTerms(options.Query)
	if len(terms) > 0 && !recordMatchesTerms(record, terms) {
		return false
	}
	return true
}

func recordMatchesTerms(record Record, terms []string) bool {
	fields := []string{
		record.ID,
		record.Provider.String(),
		record.NativeID,
		record.Title,
		record.WorkspaceRoot,
		record.ArtifactPath,
		record.ArtifactKind,
		record.Model,
	}
	joined := strings.ToLower(strings.Join(fields, "\n"))
	for _, term := range terms {
		if !strings.Contains(joined, term) {
			return false
		}
	}
	return true
}

func messageIndexTextMatchesTerms(indexText string, terms []string) bool {
	text := strings.ToLower(indexText)
	for _, term := range terms {
		if !strings.Contains(text, term) {
			return false
		}
	}
	return true
}

func queryTerms(query string) []string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(fields) == 0 {
		return nil
	}
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		term := strings.TrimSpace(field)
		if term != "" {
			terms = append(terms, term)
		}
	}
	return terms
}

func cleanWorkspaceFilter(workspaceRoot string) string {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return ""
	}
	return filepath.Clean(workspaceRoot)
}

// Snippet normalizes and bounds free text to the cross-conversation search
// snippet length so engine-first matches render like live literal matches.
func Snippet(text string) string {
	return snippet(text)
}

func snippet(text string) string {
	normalized := strings.Join(strings.Fields(text), " ")
	runes := []rune(normalized)
	if len(runes) <= searchSnippetRunes {
		return normalized
	}
	return string(runes[:searchSnippetRunes]) + "..."
}

// searchExcerptMaxBytes bounds one hit's excerpt. A ranked result carries the
// matched passage like a code-search snippet, generous enough to read in place
// but capped so the whole result list stays small for any transport. The full
// surrounding window is a separate windowed read.
const searchExcerptMaxBytes = 4096

// Excerpt bounds the matched passage to the byte budget on a rune boundary,
// preserving line breaks so a hit renders as a readable multi-line block. A
// truncated excerpt ends with an ellipsis so the reader knows to drill in for
// the rest.
func Excerpt(text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= searchExcerptMaxBytes {
		return trimmed
	}
	cut := searchExcerptMaxBytes
	for cut > 0 && !utf8.RuneStart(trimmed[cut]) {
		cut--
	}
	return strings.TrimRight(trimmed[:cut], " \t\n") + "\n..."
}

func errorsQueryRequired() error {
	return fmt.Errorf("query is required")
}
