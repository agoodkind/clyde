package conversation

import (
	"fmt"
	"strings"

	"goodkind.io/clyde/internal/search"
	"goodkind.io/clyde/internal/transcript"
)

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
