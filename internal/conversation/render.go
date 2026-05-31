package conversation

import (
	"fmt"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

// RenderPlainText renders a conversation transcript as plain text. A positive
// lastN keeps only the final lastN messages. The returned string is ready to
// print and carries the empty-state sentinel when there is nothing to show.
func RenderPlainText(record Record, lastN int) (string, error) {
	messages, err := LoadMessages(record, false, false)
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
// messageIndex. It returns a guidance sentinel when no center can be chosen.
func ContextWindowText(record Record, timestamp string, messageIndex, before, after int) (string, error) {
	messages, err := LoadMessages(record, false, false)
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
	start := max(center-before, 0)
	end := min(center+after+1, len(messages))
	return fmt.Sprintf(
		"Messages %d-%d of %d centered on %d:\n\n%s",
		start,
		end-1,
		len(messages),
		center,
		transcript.RenderPlainTextWithOptions(messages[start:end], transcript.DefaultShapeOptions()),
	), nil
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
