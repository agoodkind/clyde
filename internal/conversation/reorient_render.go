package conversation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// RenderReorientPageText formats one typed reorient page as the brief-input
// text surface shown on the CLI and MCP text responses. It renders any warnings,
// then the recovered transcript slice, then a footer telling the caller how much
// is left and how to page.
func RenderReorientPageText(page ReorientPage) string {
	var builder strings.Builder
	for _, warning := range page.Warnings {
		fmt.Fprintf(&builder, "> %s\n", warning)
	}
	if len(page.Warnings) > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString(page.Body)
	if page.Body != "" && !strings.HasSuffix(page.Body, "\n") {
		builder.WriteString("\n")
	}
	lastByte := page.Offset + len(page.Body)
	fmt.Fprintf(&builder, "---\nrecovered bytes %d-%d of %d (%d lines total)\n", page.Offset, lastByte, page.TotalBytes, page.TotalLines)
	if page.Remaining > 0 {
		fmt.Fprintf(&builder, "Not finished: call reorient again with cursor=%s. %d bytes remaining. Read every page in full before reasoning.\n", page.NextCursor, page.Remaining)
	} else {
		builder.WriteString("End of recovered transcript.\n")
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
