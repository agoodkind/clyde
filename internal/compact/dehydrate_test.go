package compact

import (
	"testing"
	"time"
)

// TestDehydrateReturnsSameSlicePointer asserts that the CLYDE-414
// pass-through implementation does not allocate a new Slice or rewrap
// the existing one. CLYDE-416 will replace this with real recomposition
// coverage.
func TestDehydrateReturnsSameSlicePointer(t *testing.T) {
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
