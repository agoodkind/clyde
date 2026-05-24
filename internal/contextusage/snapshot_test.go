package contextusage

import "testing"

// TestStaticOverheadDoesNotSubtractFreeSpace reproduces the live
// FF507951 haiku snapshot. Free space and compact buffer are not part
// of TotalTokens, so neither is subtracted from the total. The floor is
// TotalTokens minus Messages.
func TestStaticOverheadDoesNotSubtractFreeSpace(t *testing.T) {
	s := Snapshot{
		TotalTokens: 111_499,
		MaxTokens:   200_000,
		Categories: []Category{
			{Name: "System prompt", Tokens: 6_300},
			{Name: "System tools", Tokens: 22_600},
			{Name: "MCP tools", Tokens: 18_400},
			{Name: "Memory files", Tokens: 3_300},
			{Name: "Skills", Tokens: 1_400},
			{Name: "Messages", Tokens: 60_400},
			{Name: "Compact buffer", Tokens: 3_000},
			{Name: "Free space", Tokens: 84_500},
		},
	}
	got := s.StaticOverhead()
	want := 111_499 - 60_400
	if got != want {
		t.Fatalf("StaticOverhead() = %d, want %d (TotalTokens - Messages)", got, want)
	}
	if got == 0 {
		t.Fatalf("StaticOverhead() returned 0; planner would treat the session as already under target and drop nothing")
	}
}

// TestStaticOverheadFallbackSumsStaticCategories covers the path where
// the provider does not report a usable TotalTokens. The floor is the
// sum of the static categories, excluding Messages, Compact buffer, and
// Free space.
func TestStaticOverheadFallbackSumsStaticCategories(t *testing.T) {
	s := Snapshot{
		TotalTokens: 0,
		Categories: []Category{
			{Name: "System prompt", Tokens: 6_300},
			{Name: "System tools", Tokens: 22_600},
			{Name: "MCP tools", Tokens: 18_400},
			{Name: "Memory files", Tokens: 3_300},
			{Name: "Skills", Tokens: 1_400},
			{Name: "Messages", Tokens: 60_400},
			{Name: "Compact buffer", Tokens: 3_000},
			{Name: "Free space", Tokens: 84_500},
		},
	}
	got := s.StaticOverhead()
	want := 6_300 + 22_600 + 18_400 + 3_300 + 1_400
	if got != want {
		t.Fatalf("StaticOverhead() fallback = %d, want %d (sum of static categories)", got, want)
	}
}

// TestStaticOverheadClampsNegativeToZero keeps the guard that a total
// smaller than its own dynamic buckets cannot produce a negative floor.
func TestStaticOverheadClampsNegativeToZero(t *testing.T) {
	s := Snapshot{
		TotalTokens: 1_000,
		Categories: []Category{
			{Name: "Messages", Tokens: 5_000},
		},
	}
	if got := s.StaticOverhead(); got != 0 {
		t.Fatalf("StaticOverhead() = %d, want 0 when dynamic buckets exceed total", got)
	}
}
