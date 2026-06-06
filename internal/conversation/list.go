package conversation

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"goodkind.io/clyde/internal/providerid"
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
	Provider        Provider
	WorkspaceRoot   string
	IncludeArchived bool
}

// SearchMatch is the first matching message found in one conversation during
// cross-conversation discovery.
type SearchMatch struct {
	Record       Record
	MessageIndex int
	Role         string
	Timestamp    time.Time
	Snippet      string
}

// SearchConversationsResult is a bounded set of candidate conversations.
type SearchConversationsResult struct {
	Matches              []SearchMatch
	ConversationsScanned int
	ReturnedCount        int
	Limit                int
	HasMore              bool
}

// ListPage returns one filtered, bounded page from the cached index.
func (idx *Index) ListPage(ctx context.Context, options ListOptions) (ListResult, error) {
	records, err := idx.List(ctx)
	if err != nil {
		return ListResult{}, err
	}
	return FilterRecords(records, options), nil
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
			HasMore:              false,
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
		HasMore:              false,
	}
	for _, record := range candidates.Records {
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("search conversations canceled: %w", ctx.Err())
		default:
		}
		result.ConversationsScanned++
		match, ok, err := idx.firstTranscriptMatch(record, terms)
		if err != nil {
			slog.WarnContext(ctx, "conversation.search_conversations.match_failed", "concern", "conversation.search", "conversation_id", record.ID, "err", err)
			return result, fmt.Errorf("search conversation %s: %w", record.ID, err)
		}
		if !ok {
			continue
		}
		result.Matches = append(result.Matches, match)
		result.ReturnedCount = len(result.Matches)
		if result.ReturnedCount >= options.Limit {
			result.HasMore = result.ConversationsScanned < len(candidates.Records)
			return result, nil
		}
	}
	return result, nil
}

func (idx *Index) firstTranscriptMatch(record Record, terms []string) (SearchMatch, bool, error) {
	stream, err := idx.resolveStream(record, LoadOptions{
		IncludeSystemPrompts: false,
		IncludeToolOutputs:   false,
	})
	if err != nil {
		return emptySearchMatch(), false, err
	}
	messageIndex := 0
	for message, streamErr := range stream {
		if streamErr != nil {
			return emptySearchMatch(), false, streamErr
		}
		if messageMatchesTerms(message, terms) {
			return SearchMatch{
				Record:       record,
				MessageIndex: messageIndex,
				Role:         message.Role,
				Timestamp:    message.Timestamp,
				Snippet:      snippet(message.Text),
			}, true, nil
		}
		messageIndex++
	}
	return emptySearchMatch(), false, nil
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
		Record: Record{
			ID:            "",
			Provider:      providerid.ProviderUnspecified,
			NativeID:      "",
			Title:         "",
			WorkspaceRoot: "",
			ArtifactPath:  "",
			ArtifactKind:  "",
			Model:         "",
			CreatedAt:     time.Time{},
			UpdatedAt:     time.Time{},
			SizeBytes:     0,
			Archived:      false,
		},
		MessageIndex: 0,
		Role:         "",
		Timestamp:    time.Time{},
		Snippet:      "",
	}
}

func normalizeSearchConversationsOptions(options SearchConversationsOptions) SearchConversationsOptions {
	options.Query = strings.TrimSpace(options.Query)
	options.WorkspaceRoot = cleanWorkspaceFilter(options.WorkspaceRoot)
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
	if options.WorkspaceRoot != "" && cleanWorkspaceFilter(record.WorkspaceRoot) != options.WorkspaceRoot {
		return false
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

func messageMatchesTerms(message transcript.Message, terms []string) bool {
	text := strings.ToLower(message.Text)
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

func snippet(text string) string {
	normalized := strings.Join(strings.Fields(text), " ")
	runes := []rune(normalized)
	if len(runes) <= searchSnippetRunes {
		return normalized
	}
	return string(runes[:searchSnippetRunes]) + "..."
}

func errorsQueryRequired() error {
	return fmt.Errorf("query is required")
}
