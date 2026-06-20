package clispec

import (
	"strings"
	"testing"
	"time"

	conv "goodkind.io/clyde/internal/conversation"
)

func TestFormatConversationInfoShowsCountsAndSegments(t *testing.T) {
	t.Parallel()
	info := conv.Info{
		Record: conv.Record{
			ID:            "claude:info",
			Provider:      conv.ProviderClaude,
			NativeID:      "native-info",
			Lineage:       nil,
			Title:         "Info",
			WorkspaceRoot: "/repo",
			ArtifactPath:  "/tmp/info.jsonl",
			ArtifactKind:  "transcript",
			Model:         "model",
			CreatedAt:     time.Unix(1, 0),
			UpdatedAt:     time.Unix(2, 0),
			SizeBytes:     42,
			Archived:      false,
		},
		Stats: conv.Stats{
			TotalMessages:     8,
			VisibleMessages:   4,
			UserMessages:      2,
			AssistantMessages: 2,
			SystemMessages:    4,
			ToolCallCount:     3,
			ToolOutputCount:   1,
		},
		CompactionCount: 2,
		Segments: []conv.CompactionSegment{{
			Index:               0,
			StartMessageIndex:   4,
			EndMessageIndex:     8,
			HasStartingSummary:  true,
			SummaryMessageIndex: 4,
			SummaryUUID:         "summary-0",
			SummaryTimestamp:    time.Unix(3, 0),
			VisibleMessageCount: 3,
			ToolCallCount:       2,
			Checkpoint: conv.CompactionCheckpoint{
				BoundaryIndex:           -1,
				BoundaryUUID:            "",
				SummaryIndex:            4,
				SummaryUUID:             "summary-0",
				ContextItems:            nil,
				Trigger:                 "",
				HeadUUID:                "",
				AnchorUUID:              "",
				TailUUID:                "",
				MessagesSummarized:      0,
				ReplacementHistoryCount: 0,
			},
		}},
	}

	text := formatConversationInfo(info)

	for _, want := range []string{
		"conversation_id: claude:info",
		"tool_calls: 3",
		"compactions: 2",
		"segment\thas_summary\tstart_message_index",
		"0\ttrue\t4\t8\t4\t1970-01-01T00:00:03Z\t3\t2\t0",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted info missing %q in:\n%s", want, text)
		}
	}
}
