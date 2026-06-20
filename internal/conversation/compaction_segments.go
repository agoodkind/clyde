package conversation

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

const defaultCompactionSelector = "0"

// CompactionSegment describes one exportable span in a compacted conversation.
// Segment numbers count from newest to oldest, but each segment stores
// chronological message indexes.
type CompactionSegment struct {
	Index               int
	StartMessageIndex   int
	EndMessageIndex     int
	HasStartingSummary  bool
	SummaryMessageIndex int
	SummaryUUID         string
	SummaryTimestamp    time.Time
	VisibleMessageCount int
	ToolCallCount       int
	Checkpoint          CompactionCheckpoint
}

// CompactionSegmentSelection is the validated set of segments requested by an
// export selector.
type CompactionSegmentSelection struct {
	Selector string
	Segments []CompactionSegment
}

// CompactionSegments builds the newest-first segment stack for a transcript.
func CompactionSegments(messages []transcript.Message) []CompactionSegment {
	checkpoints := CompactionCheckpoints(messages)
	starts := checkpointSegmentStarts(checkpoints, len(messages))
	if len(starts) == 0 {
		return []CompactionSegment{{
			Index:               0,
			StartMessageIndex:   0,
			EndMessageIndex:     len(messages),
			HasStartingSummary:  false,
			SummaryMessageIndex: -1,
			SummaryUUID:         "",
			SummaryTimestamp:    time.Time{},
			VisibleMessageCount: countVisibleMessages(messages, 0, len(messages)),
			ToolCallCount:       countToolCalls(messages, 0, len(messages)),
			Checkpoint:          emptyCompactionCheckpoint(),
		}}
	}

	chronological := make([]CompactionSegment, 0, len(starts)+1)
	if starts[0] > 0 {
		chronological = append(chronological, CompactionSegment{
			Index:               0,
			StartMessageIndex:   0,
			EndMessageIndex:     starts[0],
			HasStartingSummary:  false,
			SummaryMessageIndex: -1,
			SummaryUUID:         "",
			SummaryTimestamp:    time.Time{},
			VisibleMessageCount: countVisibleMessages(messages, 0, starts[0]),
			ToolCallCount:       countToolCalls(messages, 0, starts[0]),
			Checkpoint:          emptyCompactionCheckpoint(),
		})
	}
	for i, checkpoint := range checkpoints {
		start := starts[i]
		end := len(messages)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		if start >= end {
			continue
		}
		summaryIndex := checkpoint.SummaryIndex
		if summaryIndex < 0 {
			summaryIndex = checkpoint.BoundaryIndex
		}
		chronological = append(chronological, CompactionSegment{
			Index:               0,
			StartMessageIndex:   start,
			EndMessageIndex:     end,
			HasStartingSummary:  summaryIndex >= 0,
			SummaryMessageIndex: summaryIndex,
			SummaryUUID:         compactionSegmentSummaryUUID(checkpoint),
			SummaryTimestamp:    compactionSegmentSummaryTimestamp(messages, summaryIndex),
			VisibleMessageCount: countVisibleMessages(messages, start, end),
			ToolCallCount:       countToolCalls(messages, start, end),
			Checkpoint:          checkpoint,
		})
	}

	for i := range chronological {
		chronological[i].Index = len(chronological) - 1 - i
	}
	segments := make([]CompactionSegment, len(chronological))
	for i, segment := range chronological {
		segments[len(chronological)-1-i] = segment
	}
	return segments
}

// SelectCompactionSegments validates a selector against the segment stack.
func SelectCompactionSegments(
	segments []CompactionSegment,
	selector string,
) (CompactionSegmentSelection, error) {
	normalized := strings.TrimSpace(selector)
	if normalized == "" {
		normalized = defaultCompactionSelector
	}
	if len(segments) == 0 {
		return CompactionSegmentSelection{}, fmt.Errorf("conversation has no compaction segments")
	}
	maxSegment := maxCompactionSegmentIndex(segments)
	indexSet, err := parseCompactionSelector(normalized, maxSegment)
	if err != nil {
		return CompactionSegmentSelection{}, err
	}
	selected := make([]CompactionSegment, 0, len(indexSet))
	for _, segment := range segments {
		if indexSet[segment.Index] {
			selected = append(selected, segment)
		}
	}
	if len(selected) == 0 {
		return CompactionSegmentSelection{}, fmt.Errorf(
			"conversation has compaction segments 0..%d",
			maxSegment,
		)
	}
	sort.SliceStable(selected, func(i int, j int) bool {
		return selected[i].Index > selected[j].Index
	})
	return CompactionSegmentSelection{
		Selector: normalized,
		Segments: selected,
	}, nil
}

func checkpointSegmentStarts(checkpoints []CompactionCheckpoint, messageCount int) []int {
	starts := make([]int, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		start := checkpointSegmentStart(checkpoint)
		if start < 0 || start >= messageCount {
			continue
		}
		starts = append(starts, start)
	}
	return starts
}

func checkpointSegmentStart(checkpoint CompactionCheckpoint) int {
	if checkpoint.SummaryIndex >= 0 {
		return checkpoint.SummaryIndex
	}
	return checkpoint.BoundaryIndex
}

func checkpointTailStart(checkpoint CompactionCheckpoint) int {
	start := checkpointSegmentStart(checkpoint)
	if start < 0 {
		return 0
	}
	return start + 1
}

func compactionSegmentSummaryUUID(checkpoint CompactionCheckpoint) string {
	if checkpoint.SummaryUUID != "" {
		return checkpoint.SummaryUUID
	}
	return checkpoint.BoundaryUUID
}

func compactionSegmentSummaryTimestamp(
	messages []transcript.Message,
	summaryIndex int,
) time.Time {
	if summaryIndex < 0 || summaryIndex >= len(messages) {
		return time.Time{}
	}
	return messages[summaryIndex].Timestamp
}

func countVisibleMessages(messages []transcript.Message, start int, end int) int {
	count := 0
	for i := start; i < end && i < len(messages); i++ {
		if messages[i].Compaction != nil {
			continue
		}
		if messages[i].Visibility == transcript.MessageVisibilityMetaOnly {
			continue
		}
		count++
	}
	return count
}

func countToolCalls(messages []transcript.Message, start int, end int) int {
	count := 0
	for i := start; i < end && i < len(messages); i++ {
		count += len(messages[i].Tools)
	}
	return count
}

func maxCompactionSegmentIndex(segments []CompactionSegment) int {
	maxIndex := 0
	for _, segment := range segments {
		if segment.Index > maxIndex {
			maxIndex = segment.Index
		}
	}
	return maxIndex
}

func parseCompactionSelector(selector string, maxSegment int) (map[int]bool, error) {
	if selector == "all" {
		selected := make(map[int]bool, maxSegment+1)
		for i := 0; i <= maxSegment; i++ {
			selected[i] = true
		}
		return selected, nil
	}
	selected := make(map[int]bool)
	for part := range strings.SplitSeq(selector, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			return nil, invalidCompactionSelectorError(selector, maxSegment)
		}
		if strings.Contains(trimmed, "..") {
			if err := parseCompactionSelectorRange(trimmed, maxSegment, selected); err != nil {
				return nil, err
			}
			continue
		}
		value, err := parseCompactionSelectorIndex(trimmed, maxSegment)
		if err != nil {
			return nil, err
		}
		selected[value] = true
	}
	return selected, nil
}

func emptyCompactionCheckpoint() CompactionCheckpoint {
	return CompactionCheckpoint{
		BoundaryIndex:           -1,
		BoundaryUUID:            "",
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

func parseCompactionSelectorRange(
	raw string,
	maxSegment int,
	selected map[int]bool,
) error {
	bounds := strings.Split(raw, "..")
	if len(bounds) != 2 || bounds[0] == "" || bounds[1] == "" {
		return invalidCompactionSelectorError(raw, maxSegment)
	}
	start, err := parseCompactionSelectorIndex(bounds[0], maxSegment)
	if err != nil {
		return err
	}
	end, err := parseCompactionSelectorIndex(bounds[1], maxSegment)
	if err != nil {
		return err
	}
	if start > end {
		return invalidCompactionSelectorError(raw, maxSegment)
	}
	for i := start; i <= end; i++ {
		selected[i] = true
	}
	return nil
}

func parseCompactionSelectorIndex(raw string, maxSegment int) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, invalidCompactionSelectorError(raw, maxSegment)
	}
	if value < 0 || value > maxSegment {
		return 0, fmt.Errorf("conversation has compaction segments 0..%d", maxSegment)
	}
	return value, nil
}

func invalidCompactionSelectorError(selector string, maxSegment int) error {
	return fmt.Errorf(
		"invalid compaction selector %q; conversation has compaction segments 0..%d",
		selector,
		maxSegment,
	)
}
