package util

import "testing"

func TestParseHumanCountAccepts(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"0", 0},
		{"200000", 200_000},
		{"200,000", 200_000},
		{"200k", 200_000},
		{"200K", 200_000},
		{"1m", 1_000_000},
		{"1M", 1_000_000},
		{"1.5m", 1_500_000},
		{"1.5k", 1_500},
		{"  50k  ", 50_000},
		{"2,000,000", 2_000_000},
	}
	for _, tc := range cases {
		got, err := ParseHumanCount(tc.in)
		if err != nil {
			t.Errorf("ParseHumanCount(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseHumanCount(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseHumanCountRejects(t *testing.T) {
	cases := []string{
		"bogus",
		"-5",
		"1.2.3",
		"5x",
		"k",
		"m",
		"0.0001k",
		"NaN",
		"Inf",
	}
	for _, in := range cases {
		got, err := ParseHumanCount(in)
		if err == nil {
			t.Errorf("ParseHumanCount(%q) = %d, want error", in, got)
		}
	}
}
