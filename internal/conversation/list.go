package conversation

import (
	"context"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
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
	// Roles, FromUnix, UntilUnix, and MinScore narrow retrieval by row
	// attributes. PerConversationLimit caps hits per conversation.
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

// SearchMatch is one matching message returned during cross-conversation
// discovery.
type SearchMatch struct {
	Record       Record
	MessageIndex int
	Role         string
	Timestamp    time.Time
	Snippet      string
	// Score is the source's retrieval relevance.
	Score float64
	// ContextWindow is the rendered messages surrounding this hit.
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
	// Source names the provider that produced the matches.
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

// snippetInputMaxBytes bounds how much leading text snippet normalizes. The
// snippet is the leading searchSnippetRunes of the message's index text, which
// now includes tool outputs that can be several megabytes. Normalizing all of it
// just to render a few hundred runes wastes CPU and allocates, and clamping the
// leading bytes cannot change the output because the leading runes come from the
// leading bytes. The budget is far larger than searchSnippetRunes*4 (the most
// bytes those runes can occupy), so a full snippet still renders.
const snippetInputMaxBytes = searchSnippetRunes * 8

func snippet(text string) string {
	if len(text) > snippetInputMaxBytes {
		cut := snippetInputMaxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut]
	}
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
