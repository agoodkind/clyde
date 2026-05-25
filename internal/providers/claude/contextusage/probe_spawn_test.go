package contextusage

import (
	"strings"
	"testing"
)

// TestBuildProbeArgs_FullOptions asserts the argv the probe spawns
// when the caller supplies a session id and a model. The probe runs
// claude in print mode with the /context slash command, pins the
// session via --resume, pins the model via --model, suppresses
// transcript writes, and caps the spawn at one non-model turn.
func TestBuildProbeArgs_FullOptions(t *testing.T) {
	args := buildProbeArgs(ProbeOptions{
		SessionID: "sess-123",
		Model:     "claude-haiku-4-5",
	})
	joined := joinArgs(args)
	for _, want := range []string{
		"-p /context",
		"--no-session-persistence",
		"--output-format json",
		"--max-turns 1",
		"--resume sess-123",
		"--model claude-haiku-4-5",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
	for _, reject := range []string{
		"--input-format",
		"--verbose",
		"--fork-session",
		"--session-id",
	} {
		if strings.Contains(joined, reject) {
			t.Fatalf("args unexpectedly contain %q: %s", reject, joined)
		}
	}
}

// TestBuildProbeArgs_OmitsEmptyFlags asserts that an empty
// SessionID omits --resume and an empty Model omits --model. claude
// without --resume reads no transcript and without --model uses its
// configured default; both are degraded but valid for a workspace-
// only probe.
func TestBuildProbeArgs_OmitsEmptyFlags(t *testing.T) {
	args := buildProbeArgs(ProbeOptions{})
	joined := joinArgs(args)
	for _, reject := range []string{
		"--resume",
		"--model",
	} {
		if strings.Contains(joined, reject) {
			t.Fatalf("args should omit %q without an opts value: %s", reject, joined)
		}
	}
	for _, want := range []string{
		"-p /context",
		"--no-session-persistence",
		"--output-format json",
		"--max-turns 1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
}

func joinArgs(args []string) string {
	var out strings.Builder
	for i, arg := range args {
		if i > 0 {
			out.WriteString(" ")
		}
		out.WriteString(arg)
	}
	return out.String()
}
