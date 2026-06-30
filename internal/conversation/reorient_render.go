package conversation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// RenderReorientPageText formats one typed reorient page as the brief-input
// text surface shown on the CLI and MCP text responses.
func RenderReorientPageText(page ReorientPage) string {
	var builder strings.Builder
	for _, item := range page.Items {
		fmt.Fprintf(&builder, "## %s\n\n%s\n\n", item.Title, item.Body)
	}
	if len(page.Items) == 0 {
		fmt.Fprintf(&builder, "---\nno items at this cursor; %d of %d delivered\n", min(page.Offset, page.TotalItems), page.TotalItems)
	} else {
		last := page.Offset + len(page.Items)
		fmt.Fprintf(&builder, "---\nitems %d-%d of %d; remaining %d\n", page.Offset+1, last, page.TotalItems, page.Remaining)
	}
	if page.Remaining > 0 {
		fmt.Fprintf(&builder, "Not finished: call reorient again with cursor=%s. Read every page before reasoning.\n", page.NextCursor)
	} else {
		builder.WriteString("All evidence delivered. You may now write the reorientation brief.\n")
	}
	return builder.String()
}

func marshalReorientPageJSON(page ReorientPage) (string, error) {
	body, err := json.MarshalIndent(page, "", "  ")
	if err != nil {
		slog.Warn("conversation.reorient.page_marshal_failed", "concern", "conversation.reorient", "component", "conversation", "err", err)
		return "", fmt.Errorf("render reorient page json: %w", err)
	}
	return string(body), nil
}
