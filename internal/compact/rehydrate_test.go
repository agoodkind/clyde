package compact

import (
	"testing"
	"time"
)

// TestRehydrateReturnsSameSlicePointer asserts that the CLYDE-413
// pass-through implementation does not allocate a new Slice or rewrap
// the existing one. CLYDE-415 will replace this with real decomposition
// coverage.
func TestRehydrateReturnsSameSlicePointer(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{
		userText("u1", "", "line one", t0),
		userText("u2", "u1", "line two", t0.Add(time.Second)),
	})

	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	got := Rehydrate(slice, 8)
	if got != slice {
		t.Fatalf("Rehydrate returned a different *Slice pointer: got %p, want %p", got, slice)
	}
}
