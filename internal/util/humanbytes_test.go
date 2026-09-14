package util

import "testing"

func TestFormatHumanBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{3000, "2.9 KB"},
		{14550997, "13.9 MB"},
		{1024 * 1024, "1.0 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := FormatHumanBytes(tc.n); got != tc.want {
				t.Fatalf("FormatHumanBytes(%d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}
