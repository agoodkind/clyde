package conversation

import (
	"goodkind.io/clyde/internal/transcript"
)

func appendSyntheticPreCompactCheckpoint(checkpoints []CompactionCheckpoint, messages []transcript.Message) []CompactionCheckpoint {
	if len(messages) == 0 {
		return checkpoints
	}
	return append(checkpoints, CompactionCheckpoint{
		BoundaryIndex:           len(messages),
		BoundaryUUID:            "",
		SummaryIndex:            -1,
		SummaryUUID:             "",
		ContextItems:            nil,
		Trigger:                 transcript.CompactionTriggerUnknown,
		HeadUUID:                "",
		AnchorUUID:              "",
		TailUUID:                "",
		MessagesSummarized:      len(ordinaryMessages(messages)),
		ReplacementHistoryCount: 0,
	})
}

func latestSyntheticPreCompactCheckpoint(checkpoints []CompactionCheckpoint) (int, *CompactionCheckpoint) {
	if len(checkpoints) == 0 {
		return 0, nil
	}
	return len(checkpoints), &checkpoints[len(checkpoints)-1]
}
