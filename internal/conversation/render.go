package conversation

import (
	"fmt"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// RenderPlainText renders a conversation transcript as plain text. A positive
// lastN keeps only the final lastN messages, which needs the whole conversation
// to count from the end, so this reads the full artifact. The returned string is
// ready to print and carries the empty-state sentinel when there is nothing to
// show.
func (idx *Index) RenderPlainText(record Record, lastN int) (string, error) {
	messages, err := idx.LoadMessages(record, false, false)
	if err != nil {
		return "", err
	}
	if lastN > 0 && lastN < len(messages) {
		messages = messages[len(messages)-lastN:]
	}
	text := transcript.RenderPlainTextWithOptions(messages, transcript.DefaultShapeOptions())
	if text == "" {
		return "No conversation messages found.", nil
	}
	return text, nil
}

// ContextWindowText renders the messages around a center point as plain text.
// The center is the message nearest timestamp when timestamp is set, otherwise
// messageIndex. When centering on a message index the read stops at the window
// end instead of consuming the whole artifact; the timestamp path reads further
// because the nearest match can lie anywhere. It returns a guidance sentinel
// when no center can be chosen.
func (idx *Index) ContextWindowText(record Record, timestamp string, messageIndex, before, after int) (string, error) {
	if timestamp == "" && messageIndex >= 0 {
		return idx.contextWindowByIndex(record, messageIndex, before, after)
	}
	messages, err := idx.LoadMessages(record, false, false)
	if err != nil {
		return "", err
	}
	if len(messages) == 0 {
		return "No conversation messages found.", nil
	}
	center := -1
	if timestamp != "" {
		center = findNearestMessage(messages, timestamp)
	}
	if center < 0 && messageIndex >= 0 && messageIndex < len(messages) {
		center = messageIndex
	}
	if center < 0 {
		return "Provide timestamp or message_index to center on.", nil
	}
	return renderContextWindow(messages, len(messages), center, before, after), nil
}

// contextWindowByIndex renders the window around messageIndex by pulling the
// stream only as far as the window end, so a context read near the top of a
// large conversation stops well before EOF. It holds at most the windowed
// messages plus the running index it has seen, never the whole conversation.
// The header reports the highest index reached rather than the absolute total,
// because computing the total would defeat the early stop.
func (idx *Index) contextWindowByIndex(record Record, messageIndex, before, after int) (string, error) {
	stream, err := idx.resolveStream(record, LoadOptions{IncludeSystemPrompts: false, IncludeToolOutputs: false})
	if err != nil {
		return "", err
	}
	start := max(messageIndex-before, 0)
	end := messageIndex + after + 1
	var window []transcript.Message
	seen := 0
	for message, streamErr := range stream {
		if streamErr != nil {
			slog.Warn("conversation.render.context_stream_failed", "concern", "conversation.render", "component", "conversation", "provider", record.Provider.String(), "conversation_id", record.ID, "err", streamErr)
			return "", fmt.Errorf("read %s conversation: %w", record.Provider.String(), streamErr)
		}
		if seen >= start && seen < end {
			window = append(window, message)
		}
		seen++
		if seen >= end {
			// The window is complete; stop pulling so the rest of the artifact is
			// never parsed.
			break
		}
	}
	if seen == 0 {
		return "No conversation messages found.", nil
	}
	if messageIndex >= seen {
		return "Provide timestamp or message_index to center on.", nil
	}
	renderedEnd := start + len(window)
	rendered := transcript.RenderPlainTextWithOptions(window, transcript.DefaultShapeOptions())
	return fmt.Sprintf(
		"Messages %d-%d centered on %d:\n\n%s",
		start,
		renderedEnd-1,
		messageIndex,
		rendered,
	), nil
}

// renderContextWindow renders the message slice around center for a conversation
// of total messages, shared by the timestamp and full-slice paths.
func renderContextWindow(messages []transcript.Message, total, center, before, after int) string {
	start := max(center-before, 0)
	end := min(center+after+1, total)
	return fmt.Sprintf(
		"Messages %d-%d of %d centered on %d:\n\n%s",
		start,
		end-1,
		total,
		center,
		transcript.RenderPlainTextWithOptions(messages[start:end], transcript.DefaultShapeOptions()),
	)
}

// findNearestMessage returns the index of the message whose timestamp is closest
// to rawTimestamp, or -1 when rawTimestamp cannot be parsed.
func findNearestMessage(messages []transcript.Message, rawTimestamp string) int {
	var target time.Time
	for _, layout := range []string{
		"2006-01-02 15:04",
		time.RFC3339,
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
	} {
		parsed, err := time.Parse(layout, rawTimestamp)
		if err == nil {
			target = parsed
			break
		}
	}
	if target.IsZero() {
		return -1
	}
	best := -1
	bestDiff := time.Duration(1<<63 - 1)
	for i, message := range messages {
		diff := message.Timestamp.Sub(target)
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			best = i
		}
	}
	return best
}
