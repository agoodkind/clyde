package conversation

import (
	"slices"

	"goodkind.io/clyde/internal/transcript"
)

// CompactionCheckpoint is the provider-neutral compacted-context view built
// from normalized transcript messages.
type CompactionCheckpoint struct {
	BoundaryIndex           int                               `json:"boundary_index"`
	BoundaryUUID            string                            `json:"boundary_uuid"`
	SummaryIndex            int                               `json:"summary_index"`
	SummaryUUID             string                            `json:"summary_uuid"`
	ContextItems            []transcript.CompactedContextItem `json:"context_items"`
	Trigger                 transcript.CompactionTrigger      `json:"trigger"`
	HeadUUID                string                            `json:"head_uuid"`
	AnchorUUID              string                            `json:"anchor_uuid"`
	TailUUID                string                            `json:"tail_uuid"`
	MessagesSummarized      int                               `json:"messages_summarized"`
	ReplacementHistoryCount int                               `json:"replacement_history_count"`
}

// CompactionCheckpoints groups normalized compaction messages into
// provider-neutral checkpoints without branching on provider-specific formats.
func CompactionCheckpoints(
	messages []transcript.Message,
) []CompactionCheckpoint {
	if len(transcript.CompactionMessages(messages)) == 0 {
		return nil
	}
	checkpoints := make([]CompactionCheckpoint, 0)
	attachedSummaries := make(map[int]struct{})
	for index, message := range messages {
		if _, ok := attachedSummaries[index]; ok {
			continue
		}
		if message.Compaction == nil {
			continue
		}
		switch message.Compaction.Kind {
		case transcript.CompactionKindBoundary:
			checkpoint := boundaryCheckpoint(index, message)
			if index+1 < len(messages) {
				nextMessage := messages[index+1]
				if isCompactionSummary(nextMessage) &&
					summaryAttachesToBoundary(message, nextMessage) {
					checkpoint = attachSummaryCheckpoint(
						checkpoint,
						index+1,
						nextMessage,
					)
					attachedSummaries[index+1] = struct{}{}
				}
			}
			checkpoints = append(checkpoints, checkpoint)
		case transcript.CompactionKindMicroboundary:
			checkpoints = append(
				checkpoints,
				boundaryCheckpoint(index, message),
			)
		case transcript.CompactionKindSummary:
			checkpoints = append(
				checkpoints,
				summaryOnlyCheckpoint(index, message),
			)
		}
	}
	return checkpoints
}

// HasUsableCompactedContext reports whether the checkpoint carries at least one
// structured compacted-context item.
func (checkpoint CompactionCheckpoint) HasUsableCompactedContext() bool {
	return len(checkpoint.ContextItems) > 0
}

// LatestReorientCheckpoint returns the checkpoint reorient should anchor on: the
// newest checkpoint that carries usable compacted context, or, when none does,
// the newest checkpoint that has a real boundary. The first result is the
// 1-based checkpoint number, matching the export numbering in
// [selectCompactionExportCheckpoint]; it is 0 when there is no checkpoint to
// anchor on. The second result is nil in that same case. The usable-first scan
// mirrors current_context export selection, so reorient and export agree on
// which checkpoint is current.
func LatestReorientCheckpoint(checkpoints []CompactionCheckpoint) (int, *CompactionCheckpoint) {
	for index, checkpoint := range slices.Backward(checkpoints) {
		if checkpoint.HasUsableCompactedContext() {
			selected := checkpoint
			return index + 1, &selected
		}
	}
	for index, checkpoint := range slices.Backward(checkpoints) {
		if checkpoint.BoundaryIndex >= 0 {
			selected := checkpoint
			return index + 1, &selected
		}
	}
	return 0, nil
}

func boundaryCheckpoint(
	index int,
	message transcript.Message,
) CompactionCheckpoint {
	metadata := message.Compaction
	if metadata == nil {
		return CompactionCheckpoint{
			BoundaryIndex:           index,
			BoundaryUUID:            message.UUID,
			SummaryIndex:            -1,
			SummaryUUID:             "",
			ContextItems:            nil,
			Trigger:                 transcript.CompactionTriggerUnknown,
			HeadUUID:                "",
			AnchorUUID:              "",
			TailUUID:                "",
			MessagesSummarized:      0,
			ReplacementHistoryCount: 0,
		}
	}
	return CompactionCheckpoint{
		BoundaryIndex:           index,
		BoundaryUUID:            message.UUID,
		SummaryIndex:            -1,
		SummaryUUID:             "",
		ContextItems:            copyContextItems(metadata.ContextItems),
		Trigger:                 metadata.Trigger,
		HeadUUID:                metadata.HeadUUID,
		AnchorUUID:              metadata.AnchorUUID,
		TailUUID:                metadata.TailUUID,
		MessagesSummarized:      metadata.MessagesSummarized,
		ReplacementHistoryCount: metadata.ReplacementHistoryCount,
	}
}

func summaryOnlyCheckpoint(
	index int,
	message transcript.Message,
) CompactionCheckpoint {
	metadata := message.Compaction
	if metadata == nil {
		return CompactionCheckpoint{
			BoundaryIndex:           -1,
			BoundaryUUID:            "",
			SummaryIndex:            index,
			SummaryUUID:             message.UUID,
			ContextItems:            nil,
			Trigger:                 transcript.CompactionTriggerUnknown,
			HeadUUID:                "",
			AnchorUUID:              "",
			TailUUID:                "",
			MessagesSummarized:      0,
			ReplacementHistoryCount: 0,
		}
	}
	return CompactionCheckpoint{
		BoundaryIndex:           -1,
		BoundaryUUID:            "",
		SummaryIndex:            index,
		SummaryUUID:             message.UUID,
		ContextItems:            copyContextItems(metadata.ContextItems),
		Trigger:                 metadata.Trigger,
		HeadUUID:                metadata.HeadUUID,
		AnchorUUID:              metadata.AnchorUUID,
		TailUUID:                metadata.TailUUID,
		MessagesSummarized:      metadata.MessagesSummarized,
		ReplacementHistoryCount: metadata.ReplacementHistoryCount,
	}
}

func attachSummaryCheckpoint(
	checkpoint CompactionCheckpoint,
	summaryIndex int,
	summary transcript.Message,
) CompactionCheckpoint {
	checkpoint.SummaryIndex = summaryIndex
	checkpoint.SummaryUUID = summary.UUID
	if summary.Compaction != nil {
		checkpoint.ContextItems = append(
			copyContextItems(checkpoint.ContextItems),
			copyContextItems(summary.Compaction.ContextItems)...,
		)
		checkpoint.MessagesSummarized = firstNonZeroInt(
			checkpoint.MessagesSummarized,
			summary.Compaction.MessagesSummarized,
		)
		checkpoint.ReplacementHistoryCount = firstNonZeroInt(
			checkpoint.ReplacementHistoryCount,
			summary.Compaction.ReplacementHistoryCount,
		)
	}
	return checkpoint
}

func isCompactionSummary(message transcript.Message) bool {
	return message.Compaction != nil &&
		message.Compaction.Kind == transcript.CompactionKindSummary
}

func summaryAttachesToBoundary(
	boundary transcript.Message,
	summary transcript.Message,
) bool {
	if summary.ParentUUID == boundary.UUID ||
		summary.LogicalParentUUID == boundary.UUID {
		return true
	}
	return summary.ParentUUID == "" && summary.LogicalParentUUID == ""
}

func copyContextItems(
	items []transcript.CompactedContextItem,
) []transcript.CompactedContextItem {
	if len(items) == 0 {
		return nil
	}
	return append([]transcript.CompactedContextItem(nil), items...)
}

func firstNonZeroInt(a int, b int) int {
	if a != 0 {
		return a
	}
	return b
}
