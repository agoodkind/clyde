package conversation

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/search"
	"goodkind.io/clyde/internal/transcript"
	"goodkind.io/clyde/internal/util"
)

// SearchResultSet is the persisted follow-up handle for a search.
type SearchResultSet struct {
	ConversationID string               `json:"conversation_id"`
	Messages       []transcript.Message `json:"messages"`
	Results        []search.Result      `json:"results"`
	CreatedAt      time.Time            `json:"created_at"`
}

// SearchOutput is the user-facing search result.
type SearchOutput struct {
	ResultID string
	Text     string
}

var searchResultCache sync.Map

// SearchConversation searches one raw conversation artifact.
func SearchConversation(
	ctx context.Context,
	record Record,
	query string,
	depth string,
	searchConfig config.SearchConfig,
) (SearchOutput, error) {
	messages, err := LoadMessages(record, false, false)
	if err != nil {
		slog.WarnContext(ctx, "conversation.search.load_failed", "concern", "conversation.search", "component", "conversation", "conversation_id", record.ID, "err", err)
		return SearchOutput{}, fmt.Errorf("load conversation: %w", err)
	}
	if len(messages) == 0 {
		return SearchOutput{}, fmt.Errorf("conversation has no messages")
	}
	results, err := search.WithDepth(ctx, messages, query, searchConfig, depth)
	if err != nil {
		slog.WarnContext(ctx, "conversation.search.messages_failed", "concern", "conversation.search", "component", "conversation", "conversation_id", record.ID, "depth", depth, "err", err)
		return SearchOutput{}, fmt.Errorf("search messages: %w", err)
	}
	if len(results) == 0 {
		return SearchOutput{ResultID: "", Text: "No matching messages found."}, nil
	}
	resultID, err := util.GenerateUUIDE()
	if err != nil {
		slog.WarnContext(ctx, "conversation.search.result_id_failed", "concern", "conversation.search", "component", "conversation", "conversation_id", record.ID, "err", err)
		return SearchOutput{}, fmt.Errorf("generate result id: %w", err)
	}
	flatMessages := flattenResultMessages(results)
	storeSearchResult(resultID, &SearchResultSet{
		ConversationID: record.ID,
		Messages:       flatMessages,
		Results:        results,
		CreatedAt:      clock.Now(),
	})
	return SearchOutput{
		ResultID: resultID,
		Text:     renderSearchResults(resultID, record, messages, results),
	}, nil
}

// AnalyzeSearchResults runs the configured local analysis model over cached search results.
func AnalyzeSearchResults(ctx context.Context, resultID string, prompt string, searchConfig config.SearchConfig) (string, error) {
	cached, ok := LoadSearchResult(resultID)
	if !ok {
		return "", fmt.Errorf("result_id %q not found", resultID)
	}
	if len(cached.Messages) == 0 {
		return "", fmt.Errorf("cached result has no messages")
	}
	var excerpts strings.Builder
	for _, message := range cached.Messages {
		role := "User"
		if message.Role == "assistant" {
			role = "Assistant"
		}
		fmt.Fprintf(&excerpts, "[%s] %s:\n%s\n\n", message.Timestamp.Format("2006-01-02 15:04"), role, message.Text)
	}
	fullPrompt := fmt.Sprintf("%s\n\nCONVERSATION EXCERPTS from conversation %q:\n\n%s", prompt, cached.ConversationID, excerpts.String())
	model := searchConfig.Local.Model
	if len(searchConfig.Local.Pipeline) > 0 {
		model = searchConfig.Local.Pipeline[0].Model
	}
	if model == "" {
		model = "qwen2.5-coder-32b"
	}
	client := search.NewClientForModel(searchConfig, model)
	response, err := client.Complete(ctx, fullPrompt)
	if err != nil {
		slog.WarnContext(ctx, "conversation.search.analysis_failed", "concern", "conversation.search", "component", "conversation", "result_id", resultID, "err", err)
		return "", fmt.Errorf("complete analysis prompt: %w", err)
	}
	return response, nil
}

// LoadSearchResult retrieves a cached result set from memory or disk.
func LoadSearchResult(resultID string) (*SearchResultSet, bool) {
	if _, err := uuid.Parse(resultID); err != nil {
		return nil, false
	}
	if value, ok := searchResultCache.Load(resultID); ok {
		cached, ok := value.(*SearchResultSet)
		return cached, ok
	}
	return nil, false
}

func storeSearchResult(resultID string, cached *SearchResultSet) {
	searchResultCache.Store(resultID, cached)
	slog.Debug("conversation.search.cache_store", "concern", "conversation.search", "component", "conversation", "result_id", resultID)
}

func flattenResultMessages(results []search.Result) []transcript.Message {
	var out []transcript.Message
	for _, result := range results {
		out = append(out, result.Messages...)
	}
	return out
}

func renderSearchResults(
	resultID string,
	record Record,
	messages []transcript.Message,
	results []search.Result,
) string {
	indexByUUID := make(map[string]int, len(messages))
	for i, message := range messages {
		if message.UUID != "" {
			indexByUUID[message.UUID] = i
		}
	}
	var out strings.Builder
	fmt.Fprintf(&out, "result_id: %s\n", resultID)
	fmt.Fprintf(&out, "conversation_id: %s\n\n", record.ID)
	for _, result := range results {
		if result.Summary != "" {
			fmt.Fprintf(&out, "Found: %s\n\n", result.Summary)
		}
		for _, message := range result.Messages {
			index, ok := indexByUUID[message.UUID]
			if !ok {
				index = -1
			}
			role := "User"
			if message.Role == "assistant" {
				role = "Assistant"
			}
			if index >= 0 {
				fmt.Fprintf(&out, "[#%d][%s] %s:\n", index, message.Timestamp.Format("2006-01-02 15:04"), role)
			} else {
				fmt.Fprintf(&out, "[%s] %s:\n", message.Timestamp.Format("2006-01-02 15:04"), role)
			}
			if message.Text != "" {
				out.WriteString(message.Text)
				out.WriteString("\n")
			}
			if message.HasTools {
				fmt.Fprintf(&out, "  [used: %s]\n", strings.Join(message.ToolNames(), ", "))
			}
			out.WriteString("\n")
		}
		out.WriteString("---\n\n")
	}
	return out.String()
}
