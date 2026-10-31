package parser_test

import (
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providers/cursor/parser"
)

const (
	chronologyOpening = "Start the retention review."
	chronologyReply   = "The retention review found three expired snapshots."
	chronologyFollow  = "Delete the expired snapshots now."
)

// chronologyCopy creates an unreferenced assistant reply bubble with a server bubble ID.
func chronologyCopy(bubbleID string, created string) fixtureBubble {
	copied := assistantBubble(bubbleID, chronologyReply, created)
	copied.ServerBubbleID = "71f3024c-3b7b-5727-8af4-51838c6c15dd"
	copied.Unreferenced = true
	return copied
}

// TestTranscriptMessagesTakeTheOnlyCandidateDateInsideTheOriginalToolInterval checks
// that the transcript reply uses the only candidate date between original tool anchors.
// It also checks that both user messages keep unknown timestamps.
func TestTranscriptMessagesTakeTheOnlyCandidateDateInsideTheOriginalToolInterval(t *testing.T) {
	rootDir, transcriptPath := boundaryRoots(t, []string{
		userLine(t, chronologyOpening),
		toolLine(t, commandArgs(t, nativeGitCommand, "")),
		assistantTextLine(t, chronologyReply),
		userLine(t, chronologyFollow),
		toolLine(t, commandArgs(t, nativeTestCommand, "")),
	})
	writeBoundaryStore(t, rootDir, []fixtureNativeMessage{
		userMessage(chronologyOpening),
		assistantMessage(callPart("call_early", commandArgs(t, nativeGitCommand, ""))),
		{Role: "assistant", Content: []fixturePart{{Type: "text", Text: chronologyReply, ToolCallID: "", ToolName: "", Args: nil}}},
		userMessage(chronologyFollow),
		assistantMessage(callPart("call_late", commandArgs(t, nativeTestCommand, ""))),
	}, nil, []fixtureBubble{
		userBubble("u1", chronologyOpening, "2026-03-29T22:30:00.000Z"),
		toolBubble("t1", "2026-03-29T22:37:46.036Z", "call_early"),
		assistantBubble("a1", chronologyReply, "2026-03-29T22:42:45.253Z"),
		chronologyCopy("a2", "2026-03-29T23:26:06.115Z"),
		chronologyCopy("a3", "2026-03-30T00:15:06.453Z"),
		chronologyCopy("a4", "2026-03-30T00:22:03.704Z"),
		chronologyCopy("a5", "2026-03-30T05:39:38.181Z"),
		userBubble("u2", chronologyFollow, "2026-03-30T05:39:38.181Z"),
		toolBubble("t2", "2026-03-29T22:45:59.728Z", "call_late"),
	})
	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, sourceRootLoadOptions()))
	if err != nil {
		t.Fatalf("cannot collect transcript messages: %v", err)
	}
	stamps := map[string]time.Time{}
	for _, message := range messages {
		if message.Text != "" {
			stamps[message.Text] = message.Timestamp
		}
	}
	if got := stamps[chronologyReply]; got.Unix() != 1774824165 {
		t.Fatalf("assistant reply timestamp differs from 1774824165; actual timestamp: %v", got)
	}
	if got := stamps[chronologyFollow]; !got.IsZero() {
		t.Fatalf("follow-up user message has a known timestamp; actual timestamp: %v", got)
	}
	if got := stamps[chronologyOpening]; !got.IsZero() {
		t.Fatalf("opening user message has a known timestamp; actual timestamp: %v", got)
	}
}
