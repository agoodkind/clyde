package contextusage

import (
	"strings"
	"testing"
)

func TestBuildProbeArgsForksByDefault(t *testing.T) {
	args := buildProbeArgs(ProbeOptions{
		SessionID:   "sess-123",
		ForkSession: true,
	})
	joined := joinArgs(args)
	for _, want := range []string{
		"-p",
		"--resume sess-123",
		"--fork-session",
		"--session-id ",
		"-n clyde-probe-",
		"--no-session-persistence",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
}

func TestBuildProbeArgsCanSkipForking(t *testing.T) {
	args := buildProbeArgs(ProbeOptions{
		SessionID:   "sess-123",
		ForkSession: false,
	})
	joined := joinArgs(args)
	if strings.Contains(joined, "--fork-session") {
		t.Fatalf("args unexpectedly enabled forking: %s", joined)
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
