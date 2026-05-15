package compact

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestParseTokenCount_Valid(t *testing.T) {
	cases := []struct {
		in  string
		out int
	}{
		{"200k", 200_000},
		{"200K", 200_000},
		{"1m", 1_000_000},
		{"1.5m", 1_500_000},
		{"0.5m", 500_000},
		{"120000", 120_000},
		{"120,000", 120_000},
		{"1,234,567", 1_234_567},
		{"  500  ", 500},
		{"500", 500},
		{"0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseTokenCount(tc.in)
			if err != nil {
				t.Fatalf("ParseTokenCount(%q) unexpected err: %v", tc.in, err)
			}
			if got != tc.out {
				t.Fatalf("ParseTokenCount(%q) = %d, want %d", tc.in, got, tc.out)
			}
		})
	}
}

func TestParseTokenCount_Invalid(t *testing.T) {
	bads := []string{
		"",
		"  ",
		"abc",
		"k",
		"m",
		"1.2.3",
		"-1",
		"-500k",
	}
	for _, s := range bads {
		t.Run(s, func(t *testing.T) {
			if _, err := ParseTokenCount(s); err == nil {
				t.Fatalf("ParseTokenCount(%q) expected error", s)
			}
		})
	}
}

func TestParseCompactTargetRequiresTarget(t *testing.T) {
	cmd := newParseTargetTestCommand(t)

	_, err := parseCompactTarget(cmd, "session", []string{"session"})
	if err == nil {
		t.Fatalf("parseCompactTarget returned nil error for omitted target")
	}
	if !strings.Contains(err.Error(), "target token count is required") {
		t.Fatalf("parseCompactTarget error = %v, want required target", err)
	}
}

func TestParseCompactTargetRejectsZero(t *testing.T) {
	cases := []struct {
		name string
		args []string
		flag string
	}{
		{name: "positional", args: []string{"session", "0"}},
		{name: "flag", args: []string{"session"}, flag: "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newParseTargetTestCommand(t)
			if tc.flag != "" {
				if err := cmd.Flags().Set("target", tc.flag); err != nil {
					t.Fatalf("set target flag: %v", err)
				}
			}

			_, err := parseCompactTarget(cmd, "session", tc.args)
			if err == nil {
				t.Fatalf("parseCompactTarget returned nil error for zero target")
			}
			if !strings.Contains(err.Error(), "target token count must be greater than zero") {
				t.Fatalf("parseCompactTarget error = %v, want positive target", err)
			}
		})
	}
}

func newParseTargetTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.Flags().String("target", "", "")
	return cmd
}
