// Token formatters for the ui package.
//
// formatTokensExact renders an integer token count with thousands
// separators, for example 1234567 -> "1,234,567". Use this where the
// caller wants the precise value visible. Current callers include the
// compact panel context summary, target/max labels, iteration logs,
// counts of messages, tools and prompts in the export panel summary,
// and the transcript total in the details pane.
//
// formatTokensCompact renders an integer token count in short
// "k/M" notation, for example 1234567 -> "1.2M". Use this where the
// caller wants a compact glance value. Current callers include the
// details pane context usage row, the per-message detail estimate,
// last-pre-compact size, and the export panel estimate row.

package ui

import (
	"fmt"
	"strconv"
	"strings"
)

// formatTokensExact returns value formatted with thousands separators.
// Zero renders as "0". Negative values keep their sign.
func formatTokensExact(value int) string {
	if value == 0 {
		return "0"
	}
	isNegative := value < 0
	if isNegative {
		value = -value
	}
	digits := strconv.Itoa(value)
	var formatted strings.Builder
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			formatted.WriteString(",")
		}
		formatted.WriteString(string(digits[i]))
	}
	if isNegative {
		return "-" + formatted.String()
	}
	return formatted.String()
}

// formatTokensCompact returns tokens in compact "k/M" notation.
// Negative values clamp to "0". Values under 1000 render with thousands
// separators via formatTokensExact so small counts read naturally.
func formatTokensCompact(tokens int) string {
	if tokens < 0 {
		return "0"
	}
	switch {
	case tokens >= 1_000_000:
		value := float64(tokens) / 1_000_000
		if tokens%1_000_000 == 0 {
			return fmt.Sprintf("%.0fM", value)
		}
		return fmt.Sprintf("%.1fM", value)
	case tokens >= 100_000:
		return fmt.Sprintf("%dk", tokens/1000)
	case tokens >= 1_000:
		value := float64(tokens) / 1_000
		if tokens%1_000 == 0 {
			return fmt.Sprintf("%.0fk", value)
		}
		return fmt.Sprintf("%.1fk", value)
	default:
		return formatTokensExact(tokens)
	}
}

// formatDetailTokens returns the "~123k tok" shape used by the details
// pane and the export panel estimate row. Non-positive inputs render
// as the placeholder "-" so empty rows stay legible.
func formatDetailTokens(tokens int) string {
	if tokens <= 0 {
		return "-"
	}
	return "~" + formatTokensCompact(tokens) + " tok"
}
