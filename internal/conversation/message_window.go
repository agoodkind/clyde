package conversation

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/transcript"
)

// ReadMessageWindow reads [start,end) under the stored loading rules. It
// stops at end and retains only messages inside the requested window.
func (idx *Index) ReadMessageWindow(ctx context.Context, record Record, start, end int, loadRules string) ([]transcript.Message, error) {
	if start < 0 || end < start {
		return nil, fmt.Errorf("invalid conversation message window %d:%d", start, end)
	}
	options, known := LoadOptionsForRules(loadRules)
	if !known {
		return nil, fmt.Errorf("unknown conversation loading rules %q", loadRules)
	}
	messages, _, err := idx.readMessageWindow(ctx, record, start, end, options)
	return messages, err
}

func (idx *Index) readMessageWindow(ctx context.Context, record Record, start, end int, options LoadOptions) ([]transcript.Message, int, error) {
	if end <= start {
		return nil, 0, nil
	}
	stream, err := idx.resolveStream(record, options)
	if err != nil {
		return nil, 0, err
	}
	var messages []transcript.Message
	position := 0
	for message, streamErr := range stream {
		if err := ctx.Err(); err != nil {
			slog.WarnContext(ctx, "conversation.context_window.cancelled", "component", "conversation", "concern", "conversation.load", "conversation_id", record.ID, "err", err)
			return nil, position, fmt.Errorf("read conversation message window: %w", err)
		}
		if streamErr != nil {
			return nil, position, streamErr
		}
		if position >= start {
			messages = append(messages, message)
		}
		position++
		if position >= end {
			break
		}
	}
	return messages, position, nil
}
