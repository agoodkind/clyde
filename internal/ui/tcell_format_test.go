package ui

import "testing"

func TestFormatTokensExact(t *testing.T) {
	cases := map[int]string{
		0:        "0",
		1:        "1",
		999:      "999",
		1000:     "1,000",
		1023:     "1,023",
		12345:    "12,345",
		999999:   "999,999",
		1000000:  "1,000,000",
		99000000: "99,000,000",
	}
	for in, want := range cases {
		if got := formatTokensExact(in); got != want {
			t.Fatalf("formatTokensExact(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatTokensCompactTable(t *testing.T) {
	cases := map[int]string{
		0:        "0",
		1:        "1",
		999:      "999",
		1000:     "1k",
		1023:     "1.0k",
		12345:    "12.3k",
		999999:   "999k",
		1000000:  "1M",
		99000000: "99M",
	}
	for in, want := range cases {
		if got := formatTokensCompact(in); got != want {
			t.Fatalf("formatTokensCompact(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestFormatExactContextUsageHidesZeroPercent confirms that when the
// daemon emits Percentage=0 the row hides the percent suffix instead of
// recomputing locally. The token totals still render so the row stays
// useful.
func TestFormatExactContextUsageHidesZeroPercent(t *testing.T) {
	usage := SessionContextUsage{
		TotalTokens: 84320,
		MaxTokens:   200000,
		Percentage:  0,
	}
	got := formatExactContextUsage(usage)
	want := "84.3k/200k tok"
	if got != want {
		t.Fatalf("formatExactContextUsage zero-percent = %q want %q", got, want)
	}
}
