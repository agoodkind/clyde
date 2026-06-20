package conversation

import (
	"strings"
	"testing"

	"goodkind.io/clyde/internal/transcript"
)

func TestCompactionSegmentsBuildNewestFirstStack(t *testing.T) {
	t.Parallel()
	segments := CompactionSegments(compactionExportMessages())

	if len(segments) != 4 {
		t.Fatalf("segments len = %d, want 4", len(segments))
	}
	assertSegment(t, segments[0], 0, 6, 8, true, "summary-3", 1)
	assertSegment(t, segments[1], 1, 3, 6, true, "micro-2", 1)
	assertSegment(t, segments[2], 2, 1, 3, true, "summary-1", 1)
	assertSegment(t, segments[3], 3, 0, 1, false, "", 0)
}

func TestCompactionSegmentsNoCompactionReturnsSingleSegment(t *testing.T) {
	t.Parallel()
	messages := []transcript.Message{
		exportChatMessage("one", "user", "first", 1),
		exportChatMessage("two", "assistant", "second", 2),
	}

	segments := CompactionSegments(messages)

	if len(segments) != 1 {
		t.Fatalf("segments len = %d, want 1", len(segments))
	}
	assertSegment(t, segments[0], 0, 0, 2, false, "", 2)
}

func TestSelectCompactionSegmentsAcceptsSupportedSelectors(t *testing.T) {
	t.Parallel()
	segments := CompactionSegments(compactionExportMessages())

	cases := []struct {
		name     string
		selector string
		want     []int
	}{
		{name: "empty defaults to zero", selector: "", want: []int{0}},
		{name: "single", selector: "0", want: []int{0}},
		{name: "list", selector: "0,1", want: []int{1, 0}},
		{name: "range", selector: "0..2", want: []int{2, 1, 0}},
		{name: "all", selector: "all", want: []int{3, 2, 1, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			selection, err := SelectCompactionSegments(segments, tc.selector)
			if err != nil {
				t.Fatalf("SelectCompactionSegments: %v", err)
			}
			assertSelectedIndexes(t, selection.Segments, tc.want)
		})
	}
}

func TestSelectCompactionSegmentsRejectsOutOfRangeSegment(t *testing.T) {
	t.Parallel()
	segments := CompactionSegments(compactionExportMessages())

	_, err := SelectCompactionSegments(segments, "4")

	if err == nil {
		t.Fatal("expected out-of-range selector error")
	}
	if !strings.Contains(err.Error(), "conversation has compaction segments 0..3") {
		t.Fatalf("error = %q, want segment range", err.Error())
	}
}

func TestCheckpointTailStartBeginsAfterCheckpointMarker(t *testing.T) {
	t.Parallel()

	withSummary := CompactionCheckpoint{
		BoundaryIndex: -1,
		SummaryIndex:  4,
	}
	if got := checkpointTailStart(withSummary); got != 5 {
		t.Fatalf("tail start with summary = %d, want 5", got)
	}

	withBoundaryOnly := CompactionCheckpoint{
		BoundaryIndex: 7,
		SummaryIndex:  -1,
	}
	if got := checkpointTailStart(withBoundaryOnly); got != 8 {
		t.Fatalf("tail start with boundary = %d, want 8", got)
	}
}

func assertSegment(
	t *testing.T,
	segment CompactionSegment,
	wantIndex int,
	wantStart int,
	wantEnd int,
	wantSummary bool,
	wantSummaryUUID string,
	wantVisible int,
) {
	t.Helper()
	if segment.Index != wantIndex {
		t.Errorf("Index = %d, want %d", segment.Index, wantIndex)
	}
	if segment.StartMessageIndex != wantStart {
		t.Errorf("StartMessageIndex = %d, want %d", segment.StartMessageIndex, wantStart)
	}
	if segment.EndMessageIndex != wantEnd {
		t.Errorf("EndMessageIndex = %d, want %d", segment.EndMessageIndex, wantEnd)
	}
	if segment.HasStartingSummary != wantSummary {
		t.Errorf("HasStartingSummary = %t, want %t", segment.HasStartingSummary, wantSummary)
	}
	if segment.SummaryUUID != wantSummaryUUID {
		t.Errorf("SummaryUUID = %q, want %q", segment.SummaryUUID, wantSummaryUUID)
	}
	if segment.VisibleMessageCount != wantVisible {
		t.Errorf("VisibleMessageCount = %d, want %d", segment.VisibleMessageCount, wantVisible)
	}
}

func assertSelectedIndexes(
	t *testing.T,
	segments []CompactionSegment,
	want []int,
) {
	t.Helper()
	if len(segments) != len(want) {
		t.Fatalf("selected len = %d, want %d", len(segments), len(want))
	}
	for i, segment := range segments {
		if segment.Index != want[i] {
			t.Errorf("selected[%d] = %d, want %d", i, segment.Index, want[i])
		}
	}
}
