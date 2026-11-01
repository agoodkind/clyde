package parser

import (
	"fmt"
	"log/slog"
	"slices"
	"time"

	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

type toolIdentityDate struct {
	at       time.Time
	explicit bool
}

type toolIdentityDates struct {
	outer map[string][]toolIdentityDate
	typed map[string][]toolIdentityDate
}

func newToolIdentityDates() *toolIdentityDates {
	return &toolIdentityDates{outer: map[string][]toolIdentityDate{}, typed: map[string][]toolIdentityDate{}}
}

func (dates *toolIdentityDates) add(bubble cursorstore.Bubble) error {
	if bubble.ToolCall == nil {
		return nil
	}
	record, err := cursorstore.DecodeToolCallBinary(bubble.ToolCall.ToolCallBinary)
	if err != nil {
		slog.Warn("providers.cursor.parser.tool_call_binary_failed", "concern", concern, "bubble_id", bubble.BubbleID, "err", err)
		return fmt.Errorf("cannot decode tool call binary for bubble %s: %w", bubble.BubbleID, err)
	}
	entry := toolIdentityDate{at: composerBubbleTimestamp(bubble.CreatedAt), explicit: false}
	if record.HasStart && record.StartedAtMs > 0 {
		entry = toolIdentityDate{at: time.UnixMilli(record.StartedAtMs).UTC(), explicit: true}
	}
	outer := bubble.ToolCall.ToolCallID
	if outer != "" {
		dates.outer[outer] = append(dates.outer[outer], entry)
	}
	seen := map[string]bool{outer: true, "": true}
	for _, typed := range []string{record.ID, record.ShellArgsID} {
		if seen[typed] {
			continue
		}
		seen[typed] = true
		dates.typed[typed] = append(dates.typed[typed], entry)
	}
	return nil
}

func (dates *toolIdentityDates) resolve() map[string][]time.Time {
	resolved := make(map[string][]time.Time, len(dates.outer)+len(dates.typed))
	for id, entries := range dates.outer {
		resolved[id] = applicableToolDates(entries, dates.typed[id])
	}
	for id, entries := range dates.typed {
		if _, outer := dates.outer[id]; !outer {
			resolved[id] = applicableToolDates(nil, entries)
		}
	}
	return resolved
}

// Explicit start timestamps take precedence even when a copied bubble supplies
// them. Without an explicit start, creation times for the outer tool call ID
// take precedence over creation times for decoded identifiers.
func applicableToolDates(outer []toolIdentityDate, typed []toolIdentityDate) []time.Time {
	explicit := toolDatesWhere(slices.Concat(outer, typed), true)
	if len(explicit) > 0 {
		return explicit
	}
	if written := toolDatesWhere(outer, false); len(written) > 0 {
		return written
	}
	return toolDatesWhere(typed, false)
}

func toolDatesWhere(entries []toolIdentityDate, explicit bool) []time.Time {
	selected := make([]time.Time, 0, len(entries))
	for _, entry := range entries {
		if entry.explicit == explicit {
			selected = append(selected, entry.at)
		}
	}
	return selected
}
