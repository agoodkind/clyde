package conversation

import (
	"fmt"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

// renderWithinResults renders a completed within-conversation search: a header
// naming the result id and conversation, an index-coverage note when the
// literal scan ran, then one block per hit with its source, score, and message
// window.
func renderWithinResults(resultID string, record Record, messages []transcript.Message, hits []withinHit, literalUsed bool) string {
	var out strings.Builder
	fmt.Fprintf(&out, "result_id: %s\n", resultID)
	fmt.Fprintf(&out, "conversation_id: %s\n", record.ID)
	semanticCount := 0
	for _, hit := range hits {
		if hit.Source == hitSourceSemantic {
			semanticCount++
		}
	}
	fmt.Fprintf(&out, "matches: %d semantic, %d literal\n", semanticCount, len(hits)-semanticCount)
	if literalUsed {
		out.WriteString("note: the semantic index does not fully cover this transcript yet; literal matches fill the gap\n")
	}
	out.WriteString("\n")
	for _, hit := range hits {
		if hit.Source == hitSourceSemantic {
			fmt.Fprintf(&out, "Match at message #%d (semantic, score %.2f):\n", hit.MessageIndex, hit.Score)
		} else {
			fmt.Fprintf(&out, "Match at message #%d (literal):\n", hit.MessageIndex)
		}
		out.WriteString(windowText(messages, hit.MessageIndex))
		out.WriteString("---\n\n")
	}
	return out.String()
}
