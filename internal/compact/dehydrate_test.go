package compact

import (
	"testing"
	"time"
)

func TestDehydrateReturnsSameSlicePointerWithoutVirtualEntries(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{
		userText("u1", "", "line one", t0),
		userText("u2", "u1", "line two", t0.Add(time.Second)),
	})

	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	opts := SynthOptions{
		ToolDefault:          ToolDetailFull,
		ToolDetailOverride:   map[string]ToolDetail{},
		DroppedChatEntries:   map[int]bool{},
		DroppedSummaryChunks: map[int]map[string]bool{},
	}
	got := Dehydrate(slice, opts)
	if got != slice {
		t.Fatalf("Dehydrate returned a different *Slice pointer: got %p, want %p", got, slice)
	}
}

func TestDehydrateMapsVirtualDropsAndCollapsesSyntheticSource(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	line := syntheticSummaryLine("s1", "b1", t0, sampleSyntheticSummaryBlocks())
	slice, err := LoadSlice(writeTranscript(t, []string{boundaryLine("b1", "", t0.Add(-time.Second)), line}))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	slice = Rehydrate(slice, 1)
	if len(slice.PostBoundary) < 2 {
		t.Fatalf("rehydrated PostBoundary too short: %d", len(slice.PostBoundary))
	}
	opts := SynthOptions{
		ToolDefault:          ToolDetailFull,
		ToolDetailOverride:   map[string]ToolDetail{},
		DroppedChatEntries:   map[int]bool{0: true, 1: true},
		DroppedSummaryChunks: map[int]map[string]bool{},
	}

	got := Dehydrate(slice, opts)

	if len(got.PostBoundary) != 1 {
		t.Fatalf("len(PostBoundary) = %d, want 1", len(got.PostBoundary))
	}
	if !got.PostBoundary[0].IsSummary {
		t.Fatalf("collapsed entry IsSummary = false, want true")
	}
	if got.PostBoundary[0].RehydratedFrom != -1 {
		t.Fatalf("collapsed entry RehydratedFrom = %d, want -1", got.PostBoundary[0].RehydratedFrom)
	}
	if opts.DroppedChatEntries[0] || opts.DroppedChatEntries[1] {
		t.Fatalf("virtual drops remained in DroppedChatEntries: %#v", opts.DroppedChatEntries)
	}
	dropped := opts.DroppedSummaryChunks[0]
	if !dropped[summaryChunkContinue] {
		t.Fatalf("DroppedSummaryChunks[0] missing %q: %#v", summaryChunkContinue, dropped)
	}
	if !dropped["tool_item:0"] {
		t.Fatalf("DroppedSummaryChunks[0] missing tool_item:0: %#v", dropped)
	}
}
