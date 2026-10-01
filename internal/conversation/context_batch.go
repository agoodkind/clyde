package conversation

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"sort"

	"goodkind.io/clyde/internal/transcript"
)

// ContextMessageWindow selects messages in [Start, End).
type ContextMessageWindow struct {
	Start int
	End   int
}

// ContextReadStats counts actual stream traversal and retained source positions.
type ContextReadStats struct {
	SourceReads      int
	MessagesVisited  int
	MessagesRetained int
	Windows          int
}

type contextWindowSelection struct {
	positions []int
	messages  []transcript.Message
}

// FreshContextWindowsParser verifies multiple windows within one source admission.
type FreshContextWindowsParser interface {
	ReadContextWindows(context.Context, string, string, []ContextMessageWindow, LoadOptions, func([][]transcript.Message) error) (ContextReadStats, error)
}

// ReadVerifiedMessageWindows checks source stability around all window callbacks.
func (idx *Index) ReadVerifiedMessageWindows(ctx context.Context, record Record, windows []ContextMessageWindow, loadRules string, visit func([][]transcript.Message) error) (stats ContextReadStats, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "conversation.context_read.failed", "component", "conversation", "concern", "conversation.load", "conversation_id", record.ID, "err", err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return stats, fmt.Errorf("read verified contexts: %w", err)
	}
	if err := validateContextWindows(windows, visit); err != nil {
		return stats, err
	}
	options, known := LoadOptionsForRules(loadRules)
	if !known {
		return stats, fmt.Errorf("unknown conversation loading rules %q", loadRules)
	}
	parser, err := idx.registry.Lookup(record.Provider)
	if err != nil {
		return stats, err
	}
	if fresh, ok := parser.(FreshContextWindowsParser); ok {
		stats, err = fresh.ReadContextWindows(ctx, record.ArtifactPath, record.Selector, windows, options, visit)
		if err != nil {
			return stats, fmt.Errorf("read fresh provider contexts: %w", err)
		}
		return stats, nil
	}
	err = WithStableContextSources(ctx, []ContextSourceFile{{Path: record.ArtifactPath, Required: true}}, func() error {
		stream, resolveErr := idx.resolveStream(record, options)
		if resolveErr != nil {
			return resolveErr
		}
		stats, err = VisitContextWindows(ctx, stream, windows, visit)
		return err
	})
	return stats, err
}

// VisitContextWindows streams once and retains only requested source positions.
func VisitContextWindows(ctx context.Context, stream iter.Seq2[transcript.Message, error], windows []ContextMessageWindow, visit func([][]transcript.Message) error) (stats ContextReadStats, err error) {
	if err := validateContextWindows(windows, visit); err != nil {
		return stats, err
	}
	stats.Windows = len(windows)
	ranges := mergedContextWindows(windows)
	selection, readErr := readContextWindowUnion(ctx, stream, ranges, &stats)
	if readErr != nil {
		return stats, readErr
	}
	if err := ctx.Err(); err != nil {
		return stats, &ContextReadError{Operation: "visit context windows", Cause: err}
	}
	selected := make([][]transcript.Message, len(windows))
	for index, window := range windows {
		start := sort.SearchInts(selection.positions, window.Start)
		end := sort.SearchInts(selection.positions, window.End)
		selected[index] = selection.messages[start:end]
	}
	return stats, visit(selected)
}

func readContextWindowUnion(ctx context.Context, stream iter.Seq2[transcript.Message, error], ranges []ContextMessageWindow, stats *ContextReadStats) (contextWindowSelection, error) {
	selection := contextWindowSelection{positions: nil, messages: nil}
	if len(ranges) == 0 {
		return selection, nil
	}
	stats.SourceReads++
	current := 0
	for message, readErr := range stream {
		if readErr != nil {
			return selection, readErr
		}
		if err := ctx.Err(); err != nil {
			return selection, &ContextReadError{Operation: "read context stream", Cause: err}
		}
		position := stats.MessagesVisited
		stats.MessagesVisited++
		for current < len(ranges) && position >= ranges[current].End {
			current++
		}
		if current == len(ranges) {
			break
		}
		if position >= ranges[current].Start {
			selection.positions = append(selection.positions, position)
			selection.messages = append(selection.messages, message)
			stats.MessagesRetained++
		}
		if stats.MessagesVisited >= ranges[len(ranges)-1].End {
			break
		}
	}
	return selection, nil
}

func validateContextWindows(windows []ContextMessageWindow, visit func([][]transcript.Message) error) error {
	if len(windows) == 0 || visit == nil {
		return errors.New("verified context requires windows and a callback")
	}
	for _, window := range windows {
		if window.Start < 0 || window.End < window.Start {
			return errors.New("invalid verified conversation message window")
		}
	}
	return nil
}

func mergedContextWindows(windows []ContextMessageWindow) []ContextMessageWindow {
	ordered := slices.Clone(windows)
	slices.SortFunc(ordered, func(left, right ContextMessageWindow) int {
		if left.Start < right.Start {
			return -1
		}
		if left.Start > right.Start {
			return 1
		}
		return 0
	})
	var merged []ContextMessageWindow
	for _, window := range ordered {
		if window.Start == window.End {
			continue
		}
		if len(merged) == 0 || merged[len(merged)-1].End < window.Start {
			merged = append(merged, window)
			continue
		}
		merged[len(merged)-1].End = max(merged[len(merged)-1].End, window.End)
	}
	return merged
}
